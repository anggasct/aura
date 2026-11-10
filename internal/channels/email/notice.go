package email

import (
	"context"
	"encoding/json"
	"strings"
)

func (a *Adapter) noticeSender(ctx context.Context, found *admission, text string) {
	var target replyReference
	if err := json.Unmarshal(found.reply, &target); err != nil || strings.TrimSpace(text) == "" || !a.recipientAllowed(target.Sender) {
		return
	}
	base := "notice:" + found.messageID
	operation := a.operationFor(&target, base, replyBody([]string{text}))
	raw, err := a.renderOperation(&operation, false)
	if err != nil {
		a.logger.WarnContext(ctx, "notice is not renderable", "component", "email", "account", a.account.Name)
		return
	}
	outcome, err := a.submit(ctx, &Submission{From: a.account.Address, To: operation.To, Message: raw}, operation.MessageID)
	if err != nil || !outcome.Succeeded {
		a.logger.WarnContext(ctx, "notice delivery failed", "component", "email", "account", a.account.Name)
	}
}
