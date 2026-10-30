//go:build durable

package restate

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func waitSupervisorRegistered(t *testing.T, supervisor *Supervisor, previousPID int, timeout time.Duration) Snapshot {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		snapshot := supervisor.Snapshot()
		if snapshot.State == SupervisionRunning && snapshot.Registered && snapshot.PID != 0 && snapshot.PID != previousPID {
			return snapshot
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("supervisor never reached a registered running child (previous pid %d): last = %+v", previousPID, supervisor.Snapshot())
	return Snapshot{}
}

func listedDeploymentURIs(t *testing.T, adminURL string) []string {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, adminURL+"/deployments", http.NoBody)
	if err != nil {
		t.Fatalf("deployments request: %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("list deployments: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatalf("read deployments: %v", err)
	}
	var listed struct {
		Deployments []struct {
			URI string `json:"uri"`
		} `json:"deployments"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(body), &listed); err != nil {
		t.Fatalf("decode deployments %q: %v", body, err)
	}
	uris := make([]string, 0, len(listed.Deployments))
	for _, deployment := range listed.Deployments {
		uris = append(uris, deployment.URI)
	}
	return uris
}

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func dataDirHolders(dataDir string) ([]string, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var holders []string
	for _, entry := range entries {
		name := entry.Name()
		if name[0] < '0' || name[0] > '9' || name == strconv.Itoa(os.Getpid()) {
			continue
		}
		raw, err := os.ReadFile("/proc/" + name + "/cmdline")
		if err != nil || !bytes.Contains(raw, []byte(dataDir)) {
			continue
		}
		holders = append(holders, name+": "+strings.ReplaceAll(string(bytes.TrimRight(raw, "\x00")), "\x00", " "))
	}
	return holders, nil
}

func requireNoDataDirHolders(t *testing.T, dataDir string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		holders, err := dataDirHolders(dataDir)
		if err != nil {
			t.Logf("process holder inspection unavailable: %v", err)
			return
		}
		if len(holders) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("processes still hold %s after shutdown: %v", dataDir, holders)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func socketInodes(pid int) (map[string]bool, error) {
	fds, err := os.ReadDir("/proc/" + strconv.Itoa(pid) + "/fd")
	if err != nil {
		return nil, err
	}
	inodes := make(map[string]bool, len(fds))
	for _, fd := range fds {
		target, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/fd/" + fd.Name())
		if err == nil && strings.HasPrefix(target, "socket:[") {
			inodes[strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")] = true
		}
	}
	return inodes, nil
}

func decodeProcNetAddr(hexAddr string) (net.IP, string, error) {
	hostHex, portHex, ok := strings.Cut(hexAddr, ":")
	if !ok {
		return nil, "", fmt.Errorf("malformed address %q", hexAddr)
	}
	raw, err := hex.DecodeString(hostHex)
	if err != nil {
		return nil, "", err
	}
	for i := 0; i+4 <= len(raw); i += 4 {
		raw[i], raw[i+1], raw[i+2], raw[i+3] = raw[i+3], raw[i+2], raw[i+1], raw[i]
	}
	port, err := strconv.ParseUint(portHex, 16, 16)
	if err != nil {
		return nil, "", err
	}
	return net.IP(raw), strconv.FormatUint(port, 10), nil
}

func listeningAddrs(pid int) ([]string, error) {
	inodes, err := socketInodes(pid)
	if err != nil {
		return nil, err
	}
	var addrs []string
	for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		raw, err := os.ReadFile(table)
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(raw), "\n")[1:] {
			fields := strings.Fields(line)
			if len(fields) < 10 || fields[3] != "0A" || !inodes[fields[9]] {
				continue
			}
			ip, port, err := decodeProcNetAddr(fields[1])
			if err != nil {
				return nil, err
			}
			addrs = append(addrs, net.JoinHostPort(ip.String(), port))
		}
	}
	return addrs, nil
}

func requireLoopbackListeners(t *testing.T, pid int, want []string) {
	t.Helper()
	addrs, err := listeningAddrs(pid)
	if err != nil {
		t.Logf("listener inspection unavailable: %v", err)
		return
	}
	seen := make(map[string]bool, len(addrs))
	for _, addr := range addrs {
		seen[addr] = true
		host, _, err := net.SplitHostPort(addr)
		if err != nil || !net.ParseIP(host).IsLoopback() {
			t.Errorf("child %d listens on non-loopback %s (all: %v)", pid, addr, addrs)
		}
	}
	for _, addr := range want {
		if !seen[addr] {
			t.Errorf("child %d does not listen on %s (all: %v)", pid, addr, addrs)
		}
	}
}

func TestListeningAddrsReportsWildcardAndLoopbackBinds(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("listener inspection reads /proc, unavailable on %s", runtime.GOOS)
	}
	config := &net.ListenConfig{}
	wildcard, err := config.Listen(t.Context(), "tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen wildcard: %v", err)
	}
	defer func() { _ = wildcard.Close() }()
	loopback, err := config.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen loopback: %v", err)
	}
	defer func() { _ = loopback.Close() }()
	addrs, err := listeningAddrs(os.Getpid())
	if err != nil {
		t.Fatalf("listeningAddrs: %v", err)
	}
	_, wildcardPort, err := net.SplitHostPort(wildcard.Addr().String())
	if err != nil {
		t.Fatalf("wildcard addr: %v", err)
	}
	wantWildcard := net.JoinHostPort("0.0.0.0", wildcardPort)
	wantLoopback := loopback.Addr().String()
	found := map[string]bool{}
	for _, addr := range addrs {
		found[addr] = true
	}
	if !found[wantWildcard] || !found[wantLoopback] {
		t.Fatalf("listeningAddrs = %v, want %s and %s", addrs, wantWildcard, wantLoopback)
	}
	host, _, err := net.SplitHostPort(wantWildcard)
	if err != nil || net.ParseIP(host).IsLoopback() {
		t.Fatalf("wildcard bind %s judged loopback", wantWildcard)
	}
}

func TestLiveSupervisorSpawnsRegistersRestartsAndDrains(t *testing.T) {
	binary := liveRestateBinary(t)
	endpoint, _ := startTestEndpoint(t)
	handlerURL := "http://" + endpoint.BoundAddr().String()
	dataDir := filepath.Join(t.TempDir(), "restate-data")
	ingressAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	adminAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	nodeAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	adminURL := "http://" + adminAddr
	supervisor, err := NewSupervisor(&SuperviseConfig{
		BinaryPath:     binary,
		DataDir:        dataDir,
		IngressURL:     "http://" + ingressAddr,
		AdminURL:       adminURL,
		HandlerURL:     handlerURL,
		NodeAddr:       nodeAddr,
		ReadyTimeout:   60 * time.Second,
		ShutdownGrace:  10 * time.Second,
		RestartInitial: 100 * time.Millisecond,
		RestartMax:     time.Second,
	})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- supervisor.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		killHolders(dataDir)
	})

	first := waitSupervisorRegistered(t, supervisor, 0, 90*time.Second)
	requireLoopbackListeners(t, first.PID, []string{ingressAddr, adminAddr, nodeAddr})
	uris := listedDeploymentURIs(t, adminURL)
	registered := false
	for _, uri := range uris {
		if strings.Contains(uri, endpoint.BoundAddr().String()) {
			registered = true
		}
	}
	if !registered {
		t.Fatalf("deployments = %v, want one pointing at %s", uris, handlerURL)
	}

	if err := syscall.Kill(first.PID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill child %d: %v", first.PID, err)
	}
	second := waitSupervisorRegistered(t, supervisor, first.PID, 90*time.Second)
	if second.Restarts < 1 {
		t.Errorf("restarts after child crash = %d, want >= 1", second.Restarts)
	}
	if processAlive(first.PID) {
		t.Errorf("crashed child %d is still alive", first.PID)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start after cancel = %v, want nil", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("supervisor did not stop within 30s of cancel")
	}
	if state := supervisor.Snapshot().State; state != SupervisionStopped {
		t.Errorf("state after drain = %q, want %q", state, SupervisionStopped)
	}
	if processAlive(second.PID) {
		t.Errorf("child %d survived the drain", second.PID)
	}
	requireNoDataDirHolders(t, dataDir, 15*time.Second)
}
