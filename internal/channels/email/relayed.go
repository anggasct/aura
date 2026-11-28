package email

import (
	"context"
	"regexp"
	"strings"
	"sync"

	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
)

const (
	detailEmailForward         = "email forward"
	detailEmailQuote           = "email quote"
	maxRecordedDeliverySession = 256
	maxRecordedBodiesPerSess   = 64
)

type DeliveryHistory interface {
	SessionDeliveries(ctx context.Context, sessionID string) ([]string, error)
}

type recordedDeliveries struct {
	mu       sync.RWMutex
	order    []string
	bySessID map[string][]string
}

func (d *recordedDeliveries) record(sessionID, body string) {
	if sessionID == "" || strings.TrimSpace(body) == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.bySessID == nil {
		d.bySessID = map[string][]string{}
	}
	if _, exists := d.bySessID[sessionID]; !exists {
		d.order = append(d.order, sessionID)
		for len(d.order) > maxRecordedDeliverySession {
			evict := d.order[0]
			d.order = d.order[1:]
			delete(d.bySessID, evict)
		}
	}
	items := d.bySessID[sessionID]
	items = append(items, body)
	if len(items) > maxRecordedBodiesPerSess {
		items = items[len(items)-maxRecordedBodiesPerSess:]
	}
	d.bySessID[sessionID] = items
}

func (d *recordedDeliveries) list(sessionID string) []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	items := d.bySessID[sessionID]
	if len(items) == 0 {
		return nil
	}
	out := make([]string, len(items))
	copy(out, items)
	return out
}

var (
	reSubjectPrefixRe   = regexp.MustCompile(`(?i)^\s*re\s*:\s*`)
	reSubjectPrefixFwd  = regexp.MustCompile(`(?i)^(fwd|fw|wg|tr|rv|i)\s*:`)
	reForwardHeaderLine = regexp.MustCompile(`(?i)^(?:-{3,}\s?(?:forwarded message|original message|pesan terusan|pesan asli)\s?-{3,}|begin forwarded message:)$`)
	reAttributionLine   = regexp.MustCompile(`(?i)(?:wrote|menulis):\s*$`)
	reHeaderLine        = regexp.MustCompile(`^(?i)(from|sent|date|to|cc|subject|dari|dikirim|tanggal|kepada|subjek|von|gesendet|an|betreff|de|envoyé|à|objet)\s*:\s*.+$`)
	reEmailAddress      = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
)

func hasForwardSubjectPrefix(subject string) bool {
	s := strings.TrimSpace(subject)
	for {
		next := reSubjectPrefixRe.ReplaceAllString(s, "")
		if next == s {
			break
		}
		s = strings.TrimSpace(next)
	}
	return reSubjectPrefixFwd.MatchString(s)
}

func isForwardHeader(line string) bool {
	return reForwardHeaderLine.MatchString(strings.TrimSpace(line))
}

func isAttributionLine(line string) bool {
	return reAttributionLine.MatchString(strings.TrimSpace(line))
}

func isQuoteLine(line string) bool {
	return strings.HasPrefix(strings.TrimLeft(line, " \t"), ">")
}

func isHeaderBlockStart(lines []string, idx int) bool {
	if !reHeaderLine.MatchString(strings.TrimSpace(lines[idx])) {
		return false
	}
	end := min(idx+6, len(lines))
	headerCount := 0
	hasEmail := false
	for j := idx; j < end; j++ {
		trimmed := strings.TrimSpace(lines[j])
		if reHeaderLine.MatchString(trimmed) {
			headerCount++
			if reEmailAddress.MatchString(trimmed) {
				hasEmail = true
			}
		}
	}
	return headerCount >= 3 && hasEmail
}

func stripQuotePrefix(line string) string {
	s := strings.TrimLeft(line, " \t")
	for strings.HasPrefix(s, ">") {
		s = strings.TrimPrefix(s, ">")
		s = strings.TrimPrefix(s, " ")
	}
	return s
}

func normalizeForQuoteMatch(text string) string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func buildDeliverySet(deliveries []string) map[string]struct{} {
	if len(deliveries) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(deliveries))
	for _, item := range deliveries {
		if norm := normalizeForQuoteMatch(item); norm != "" {
			set[norm] = struct{}{}
		}
	}
	return set
}

func quoteBlockIsExempt(lines []string, deliverySet map[string]struct{}) bool {
	if len(deliverySet) == 0 {
		return false
	}
	unquoted := make([]string, len(lines))
	for i, line := range lines {
		unquoted[i] = stripQuotePrefix(line)
	}
	norm := normalizeForQuoteMatch(strings.Join(unquoted, "\n"))
	if norm == "" {
		return false
	}
	_, ok := deliverySet[norm]
	return ok
}

type bodySegment struct {
	text   string
	source runtimeingress.PartSource
	detail string
}

func splitEmailBody(subject, body string, deliveries []string) []runtimeingress.InputPart {
	subjectForward := hasForwardSubjectPrefix(subject)
	if body == "" {
		if subject == "" {
			return nil
		}
		text := turnInput(subject, "")
		if subjectForward {
			return []runtimeingress.InputPart{{
				Text:   text,
				Source: runtimeingress.PartSourceRelayed,
				Detail: detailEmailForward,
			}}
		}
		return []runtimeingress.InputPart{{Text: text}}
	}

	deliverySet := buildDeliverySet(deliveries)
	lines := strings.Split(body, "\n")
	var segments []bodySegment
	var authoredBuf []string
	hasRelayed := false

	flushAuthored := func() {
		if len(authoredBuf) == 0 {
			return
		}
		joined := strings.TrimSpace(strings.Join(authoredBuf, "\n"))
		authoredBuf = authoredBuf[:0]
		if joined != "" {
			segments = append(segments, bodySegment{text: joined})
		}
	}

	appendRelayed := func(text, detail string) {
		trimmed := strings.TrimSpace(text)
		if trimmed == "" {
			return
		}
		hasRelayed = true
		segments = append(segments, bodySegment{
			text:   trimmed,
			source: runtimeingress.PartSourceRelayed,
			detail: detail,
		})
	}

	for i := 0; i < len(lines); {
		line := lines[i]
		if isForwardHeader(line) || isHeaderBlockStart(lines, i) {
			flushAuthored()
			appendRelayed(strings.Join(lines[i:], "\n"), detailEmailForward)
			break
		}
		if isAttributionLine(line) {
			next := i + 1
			for next < len(lines) && strings.TrimSpace(lines[next]) == "" {
				next++
			}
			if next < len(lines) && isQuoteLine(lines[next]) {
				qEnd := next
				for qEnd < len(lines) && isQuoteLine(lines[qEnd]) {
					qEnd++
				}
				if quoteBlockIsExempt(lines[next:qEnd], deliverySet) {
					authoredBuf = append(authoredBuf, lines[i:qEnd]...)
					i = qEnd
					continue
				}
			}
			flushAuthored()
			appendRelayed(strings.Join(lines[i:], "\n"), detailEmailQuote)
			break
		}
		if isQuoteLine(line) {
			qEnd := i
			for qEnd < len(lines) && isQuoteLine(lines[qEnd]) {
				qEnd++
			}
			if quoteBlockIsExempt(lines[i:qEnd], deliverySet) {
				authoredBuf = append(authoredBuf, lines[i:qEnd]...)
			} else {
				flushAuthored()
				appendRelayed(strings.Join(lines[i:qEnd], "\n"), detailEmailQuote)
			}
			i = qEnd
			continue
		}
		authoredBuf = append(authoredBuf, line)
		i++
	}
	flushAuthored()

	if !hasRelayed {
		if subjectForward {
			return []runtimeingress.InputPart{{
				Text:   turnInput(subject, body),
				Source: runtimeingress.PartSourceRelayed,
				Detail: detailEmailForward,
			}}
		}
		return []runtimeingress.InputPart{{Text: turnInput(subject, body)}}
	}

	parts := make([]runtimeingress.InputPart, 0, len(segments)+1)
	subjectAdded := false
	if subjectForward && subject != "" {
		parts = append(parts, runtimeingress.InputPart{
			Text:   truncateInput("Subject: " + subject),
			Source: runtimeingress.PartSourceRelayed,
			Detail: detailEmailForward,
		})
		subjectAdded = true
	}
	for _, seg := range segments {
		if seg.source == "" && !subjectAdded && subject != "" {
			parts = append(parts, runtimeingress.InputPart{Text: turnInput(subject, seg.text)})
			subjectAdded = true
			continue
		}
		if seg.source != "" && !subjectAdded && subject != "" {
			parts = append(parts, runtimeingress.InputPart{Text: truncateInput("Subject: " + subject)})
			subjectAdded = true
		}
		parts = append(parts, runtimeingress.InputPart{
			Text:   truncateInput(seg.text),
			Source: seg.source,
			Detail: seg.detail,
		})
	}
	return parts
}

func truncateInput(text string) string {
	if len(text) > maxTurnInputBytes {
		return truncateUTF8(text, maxTurnInputBytes) + truncationNotice
	}
	return text
}

func emailAttachmentDetail(mediaType string) string {
	switch {
	case strings.HasPrefix(mediaType, "image/"):
		return "image"
	case strings.HasPrefix(mediaType, "audio/"):
		return "audio"
	case strings.HasPrefix(mediaType, "video/"):
		return "video"
	default:
		return "document"
	}
}
