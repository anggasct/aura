package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/anggasct/aura/internal/platform/clikit"
)

const LiveProbeWait = time.Second

type DaemonSandbox int

const (
	DaemonSandboxUnknown DaemonSandbox = iota
	DaemonSandboxUp
	DaemonSandboxDown
)

const DaemonSandboxUnavailableCode = ComponentSandbox + ".sandbox_unavailable"

func DaemonSandboxState(live ProbeBody, reachable bool) DaemonSandbox {
	switch {
	case !reachable:
		return DaemonSandboxUnknown
	case live.Status == ProbeStatusReady:
		return DaemonSandboxUp
	case live.Code == DaemonSandboxUnavailableCode:
		return DaemonSandboxDown
	default:
		return DaemonSandboxUnknown
	}
}

func LiveReadiness(ctx context.Context, offline bool, listen string) (live ProbeBody, reachable bool) {
	if offline || listen == "" {
		return ProbeBody{}, false
	}
	probeCtx, cancel := context.WithTimeout(ctx, LiveProbeWait)
	defer cancel()
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, "http://"+listen+"/readyz", http.NoBody)
	if err != nil {
		return ProbeBody{}, false
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return ProbeBody{}, false
	}
	defer func() { _ = response.Body.Close() }()
	var body ProbeBody
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return ProbeBody{}, false
	}
	return body, true
}

func RunProcessProbe(ctx context.Context, out io.Writer, listen, probe string) error {
	path, ok := map[string]string{"live": "/livez", "ready": "/readyz"}[probe]
	if !ok {
		return clikit.NewExitCodeError(ExitCommand, fmt.Errorf("--probe %q must be live or ready", probe))
	}
	if listen == "" {
		return clikit.NewExitCodeError(ExitCommand, errors.New("health.listen is not configured"))
	}
	probeCtx, cancel := context.WithTimeout(ctx, 3*LiveProbeWait)
	defer cancel()
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, "http://"+listen+path, http.NoBody)
	if err != nil {
		return clikit.NewExitCodeError(ExitCommand, err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		_, _ = fmt.Fprintln(out, probe+": unreachable")
		return clikit.NewExitCodeError(ExitCritical, errors.New("the running process is unreachable"))
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = fmt.Fprintln(out, probe+": no")
		return clikit.NewExitCodeError(ExitCritical, fmt.Errorf("the running process failed its %s probe (%s)", probe, response.Status))
	}
	_, err = fmt.Fprintln(out, probe+": yes")
	return err
}
