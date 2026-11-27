package app

import (
	"testing"
	"time"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/channels/discord"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/usage"
)

func TestPrincipalBudgetGateReadsTheTurnLedger(t *testing.T) {
	cfg := writeChildrenCLIConfig(t)
	db := openAccessDB(t, cfg)
	loaded, err := appconfig.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	loaded.Config.Access.Budgets.Public.MaxTokensPerDay = 150
	prices := usage.NewPriceRegistry()
	if err := prices.Put(&usage.Price{
		ModelDefinitionID: "primary", Currency: loaded.Config.Usage.Currency, MicrosPerInputToken: 1, MicrosPerOutputToken: 1,
		MaxReservationRate: 100, EffectiveFrom: time.Unix(0, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	ledger, err := usage.NewTurnLedger(loaded.Config.Usage, loaded.Config.Access, db, prices, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Reserve(t.Context(), &usage.ReserveRequest{
		InvocationID: "inv-1", ModelDefinitionID: "primary", PrincipalID: "discord:9", PrincipalClass: "public",
		KnownInputTokens: 50, RequestedMaxOutputTokens: 100,
	}); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if _, err := ledger.Reserve(t.Context(), &usage.ReserveRequest{
		InvocationID: "inv-2", ModelDefinitionID: "primary", PrincipalID: "discord:9", PrincipalClass: "public",
		KnownInputTokens: 1, RequestedMaxOutputTokens: 1,
	}); err == nil {
		t.Fatal("turn ledger ignored the configured public cap")
	}
	settings := channelAccessSettings(loaded.Config, db, nil, nil)
	if settings.Budgets == nil {
		t.Fatal("discord access settings carry no budget gate")
	}
	if got, err := settings.Budgets.Admit(t.Context(), "discord:9", principal.ClassPublic); err != nil || got != discord.BudgetRefusalExhausted {
		t.Fatalf("Admit() = %q, %v; want budget_exhausted from the shared ledger", got, err)
	}
}
