package profile

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
)

func (m *Module) TurnContext(ctx context.Context, req *turn.TurnRequest) ([]turn.ContextBlock, error) {
	if m == nil || m.service == nil || req == nil || req.PrincipalClass != principal.ClassOwner {
		return nil, nil
	}
	part, err := m.service.Retrieve(ctx, &RetrieveQuery{
		OwnerID:      principal.OwnerID,
		MaxFacts:     m.maxFacts,
		MaxTokens:    m.maxTokens,
		VerifiedOnly: true,
		StatedKeys:   m.stated,
	}, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	block := strings.TrimSpace(part.Render())
	if block == "" {
		return nil, nil
	}
	return []turn.ContextBlock{{
		Order:   turn.ContextOrderProfile,
		Content: block,
	}}, nil
}

func (*Module) Required(*turn.TurnRequest) bool {
	return false
}

func StatedOwnerKeys(owner *principal.Owner) []string {
	if owner == nil {
		return nil
	}
	var keys []string
	for key, value := range map[string]string{"name": owner.Name, "language": owner.Language, "timezone": owner.Timezone} {
		if strings.TrimSpace(value) != "" {
			keys = append(keys, key)
		}
	}
	for key, value := range owner.Preferences {
		if strings.TrimSpace(key) != "" && strings.TrimSpace(value) != "" {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}
