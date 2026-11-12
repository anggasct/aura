package app

import (
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/durable/sqlite"
)

func TestServerStartFailsWhenTheDurableHostHasNoLease(t *testing.T) {
	restoreDefaultLogger(t)
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	path := writeCompositionConfig(t, freeLoopbackAddress(t), compositionModelsYAML, "")
	srv := buildCompositionServer(t, path)
	refuse := `CREATE TRIGGER refuse_lease BEFORE INSERT ON durable_host BEGIN SELECT RAISE(ABORT, 'lease refused'); END`
	if _, err := compositionDatabase(t, path).ExecContext(t.Context(), refuse); err != nil {
		t.Fatalf("refuse the lease: %v", err)
	}
	srv.run(t)
	select {
	case err := <-srv.ran:
		srv.ran = nil
		if code, ok := sqlite.CodeOf(err); !ok || code != sqlite.ErrorCodeHostLease {
			t.Fatalf("Run = %v, want %s", err, sqlite.ErrorCodeHostLease)
		}
	case <-time.After(compositionServerWait):
		t.Fatal("the server serves without a durable host")
	}
	closed := make(chan error, 1)
	go func() { closed <- srv.app.Close(t.Context()) }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(compositionServerWait):
		t.Fatal("Close did not return after a failed start")
	}
}
