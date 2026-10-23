package inference

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	pb "github.com/liquiditylab/liquiditylab/gen/go/liquiditylab/v1"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
	"github.com/liquiditylab/liquiditylab/internal/replay"
	"google.golang.org/protobuf/proto"
)

// These process helpers keep the opt-in boundary check isolated from the normal
// Go suite. Both Python training and its owned replay subprocess share a process
// group, so a failed test cannot strand a worker or replay child.
type boundaryLog struct {
	mu   sync.Mutex
	tail []byte
}

func (l *boundaryLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tail = append(l.tail, p...)
	if len(l.tail) > 32<<10 {
		l.tail = append([]byte(nil), l.tail[len(l.tail)-(32<<10):]...)
	}
	return len(p), nil
}

func (l *boundaryLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return string(l.tail)
}

type boundaryProcess struct {
	command *exec.Cmd
	done    chan struct{}
	err     error // published by closing done
	log     boundaryLog
}

func startBoundaryProcess(t *testing.T, root, interpreter string, args ...string) *boundaryProcess {
	t.Helper()
	p := &boundaryProcess{command: exec.Command(interpreter, args...), done: make(chan struct{})}
	p.command.Dir = root
	p.command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	p.command.WaitDelay = 2 * time.Second
	environment := map[string]string{
		"PYTHONPATH": filepath.Join(root, "src"), "PYTHONNOUSERSITE": "1",
		"PYTHONUNBUFFERED": "1", "PYTHONDONTWRITEBYTECODE": "1",
		"OMP_NUM_THREADS": "1", "MKL_NUM_THREADS": "1", "OPENBLAS_NUM_THREADS": "1",
	}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if _, overridden := environment[name]; !overridden {
			p.command.Env = append(p.command.Env, entry)
		}
	}
	for name, value := range environment {
		p.command.Env = append(p.command.Env, name+"="+value)
	}
	p.command.Stdout, p.command.Stderr = &p.log, &p.log
	if err := p.command.Start(); err != nil {
		t.Fatalf("start Python boundary process: %v", err)
	}
	go func() { p.err = p.command.Wait(); close(p.done) }()
	t.Cleanup(func() { p.stop(t) })
	return p
}

func (p *boundaryProcess) stop(t *testing.T) {
	t.Helper()
	select {
	case <-p.done:
	default:
		_ = syscall.Kill(-p.command.Process.Pid, syscall.SIGINT)
		select {
		case <-p.done:
		case <-time.After(3 * time.Second):
		}
	}
	// Also terminate any descendant that outlived a completed Python parent.
	_ = syscall.Kill(-p.command.Process.Pid, syscall.SIGKILL)
	select {
	case <-p.done:
	case <-time.After(3 * time.Second):
		t.Error("Python boundary process did not exit after group termination")
	}
}

func boundaryExecutable(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if !filepath.IsAbs(value) {
		t.Fatalf("%s must name an absolute executable path", name)
	}
	info, err := os.Stat(value)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		t.Fatalf("%s does not name an available executable", name)
	}
	return value
}

// TestPythonPPOWorkerBoundary trains and loads a real tiny PPO bundle, then sends
// the shared Go observation through the actual Python gRPC service. It is an
// integration/protocol check and provides no policy evaluation evidence.
func TestPythonPPOWorkerBoundary(t *testing.T) {
	if os.Getenv("LIQUIDITYLAB_PYTHON") == "" || os.Getenv("LIQUIDITYLAB_REPLAY_BINARY") == "" {
		t.Skip("set absolute LIQUIDITYLAB_PYTHON and LIQUIDITYLAB_REPLAY_BINARY for the real PPO boundary integration")
	}
	interpreter := boundaryExecutable(t, "LIQUIDITYLAB_PYTHON")
	binary := boundaryExecutable(t, "LIQUIDITYLAB_REPLAY_BINARY")
	working, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Clean(filepath.Join(working, "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "pyproject.toml")); err != nil {
		t.Fatalf("cannot derive repository root from inference package: %v", err)
	}
	// /tmp keeps the Unix address short even under deeply nested Go temp roots.
	private, err := os.MkdirTemp("/tmp", "ll-ppo-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(private) })
	if err := os.Chmod(private, 0700); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(private, "bundle")
	training := startBoundaryProcess(t, root, interpreter,
		"-m", "liquiditylab.training", filepath.Join(root, "examples", "training-tape.jsonl"),
		"--plan", filepath.Join(root, "examples", "buy-plan.json"),
		"--backend", "go", "--replay-binary", binary, "--num-envs", "1",
		"--timesteps", "8", "--seed", "11", "--output", bundle)
	select {
	case <-training.done:
		if training.err != nil {
			t.Fatalf("tiny real PPO training failed: %v\n%s", training.err, training.log.String())
		}
	case <-time.After(2 * time.Minute):
		t.Fatal("tiny real PPO training exceeded two-minute integration budget")
	}
	encoded, err := os.ReadFile(filepath.Join(bundle, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		PolicySHA256 string `json:"policy_sha256"`
		Status       string `json:"status"`
		LiveEligible bool   `json:"live_eligible"`
		Backend      string `json:"backend"`
	}
	if err := json.Unmarshal(encoded, &manifest); err != nil || !validDigest(manifest.PolicySHA256) || manifest.Status != "unvalidated" || manifest.LiveEligible || manifest.Backend != "go" {
		t.Fatalf("tiny training did not produce a pinned unvalidated Go bundle: %v", err)
	}
	socket := filepath.Join(private, "worker.sock")
	worker := startBoundaryProcess(t, root, interpreter, "-m", "liquiditylab.inference", bundle, "--listen", "unix:"+socket)
	startup, cancelStartup := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelStartup()
	var client *Client
	var lastErr error
	for client == nil {
		select {
		case <-worker.done:
			t.Fatalf("real Python worker exited during startup: %v\n%s", worker.err, worker.log.String())
		case <-startup.Done():
			t.Fatalf("real Python worker was not ready in startup budget: %v\n%s", lastErr, worker.log.String())
		default:
		}
		attempt, cancelAttempt := context.WithTimeout(startup, 300*time.Millisecond)
		client, lastErr = New(attempt, Config{Target: "unix:" + socket, PolicySHA256: manifest.PolicySHA256, Profile: "paper", AllowUnvalidated: true, Timeout: 150 * time.Millisecond})
		cancelAttempt()
		if client == nil {
			select {
			case <-startup.Done():
			case <-time.After(25 * time.Millisecond):
			}
		}
	}
	t.Cleanup(func() { client.Close() })
	engine, err := replay.New(replay.SyntheticEvents(), d.DefaultPlan(), d.DefaultRules(), d.DefaultFees(), d.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	initial, err := engine.Reset()
	if err != nil {
		t.Fatal(err)
	}
	request := &pb.DecisionRequest{
		Binding: &pb.DecisionBinding{
			RequestId: "real-python-request", TenantId: "integration", AccountId: "paper-account", SessionId: "paper-session",
			SessionRevision: 1, AccountRevision: 1, PolicySha256: manifest.PolicySHA256,
			ObservationVersion: x.ObservationVersion, ObservationSchemaSha256: SchemaSHA256(),
			MarketSnapshotId: "real-go-initial-snapshot", ExpiresAtUnixNs: time.Now().Add(180 * time.Millisecond).UnixNano(),
		},
		Observation: &pb.Observation{SchemaVersion: x.ObservationVersion, FieldNames: append([]string(nil), x.ObservationFields...), Features: initial.Observation},
	}
	proposalCtx, cancelProposal := context.WithTimeout(context.Background(), time.Second)
	defer cancelProposal()
	proposal, err := client.Propose(proposalCtx, request)
	if err != nil {
		t.Fatalf("Go client -> real Python PPO inference failed: %v\n%s", err, worker.log.String())
	}
	if proposal.Action.Validate() != nil || proposal.Mode != "unvalidated" || !proto.Equal(proposal.Binding, request.Binding) {
		t.Fatalf("real Python proposal has incompatible action/mode/binding: action=%v mode=%q", proposal.Action, proposal.Mode)
	}
	if err := proposal.ValidateCurrent(Current{
		TenantID: "integration", AccountID: "paper-account", SessionID: "paper-session", SessionRevision: 1, AccountRevision: 1,
		PolicySHA256: manifest.PolicySHA256, Active: true, Ready: true, MarketValid: true,
	}, time.Now()); err != nil {
		t.Fatalf("real worker proposal failed account authority validation: %v", err)
	}
	transition, err := engine.Step(proposal.Action)
	if err != nil || transition.Truncated {
		t.Fatalf("real Python semantic action failed Go replay compilation: %v truncated=%v", err, transition.Truncated)
	}
	t.Logf("real Go/Python PPO boundary passed with %d features and semantic action %d", len(initial.Observation), proposal.Action)
}
