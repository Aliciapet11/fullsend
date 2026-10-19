package install

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// idleTraceCollector returns a started-looking collector with no
// containers, so Stop runs without podman.
func idleTraceCollector(t *testing.T) *traceCollector {
	t.Helper()
	return &traceCollector{
		logf:     t.Logf,
		token:    strings.Repeat("t", 64),
		endpoint: "https://example-tunnel.trycloudflare.com/v1/traces",
	}
}

func TestTraceStart_MintTracing(t *testing.T) {
	t.Run("no collector", func(t *testing.T) {
		endpoint, file, cleanup, err := traceStart{}.mintTracing()
		require.NoError(t, err)
		cleanup()
		assert.Empty(t, endpoint)
		assert.Empty(t, file)
	})
	t.Run("running collector", func(t *testing.T) {
		tc := idleTraceCollector(t)
		endpoint, file, cleanup, err := traceStart{started: tc}.mintTracing()
		require.NoError(t, err)
		assert.Equal(t, tc.endpoint, endpoint)

		data, err := os.ReadFile(file)
		require.NoError(t, err)
		assert.Equal(t, "Authorization=Bearer%20"+tc.token+"\n", string(data),
			"the mint gets the same bearer token the workflows use")
		if runtime.GOOS != "windows" {
			info, err := os.Stat(file)
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "the CLI requires a private headers file")
		}

		cleanup()
		_, err = os.Stat(file)
		assert.True(t, os.IsNotExist(err), "cleanup removes the token file")
	})
}

func TestTraceStart_Attach(t *testing.T) {
	t.Run("composed driver takes ownership", func(t *testing.T) {
		tc := idleTraceCollector(t)
		pending := &fakeTraceSink{}
		d, err := newComposedDriver("org", &fakeMintDriver{}, newFakeEnsurer(), 1, t.Logf)
		require.NoError(t, err)

		got := traceStart{started: tc, pending: pending}.attach(d)
		cd := got.(*composedDriver)
		assert.Same(t, tc, cd.traces)
		assert.Same(t, pending, cd.pendingTraces)
		assert.Equal(t, 0, pending.stopCalls)
	})
	t.Run("non-composed driver stops the collector", func(t *testing.T) {
		pending := &fakeTraceSink{}
		inner, err := newComposedDriver("org", &fakeMintDriver{}, newFakeEnsurer(), 1, t.Logf)
		require.NoError(t, err)
		pd := NewPlaybackDriver(inner)

		assert.Same(t, pd, traceStart{pending: pending}.attach(pd))
		assert.Equal(t, 1, pending.stopCalls)
	})
	t.Run("nothing started", func(t *testing.T) {
		d, err := newComposedDriver("org", &fakeMintDriver{}, newFakeEnsurer(), 1, t.Logf)
		require.NoError(t, err)
		cd := traceStart{}.attach(d).(*composedDriver)
		assert.Nil(t, cd.traces)
		assert.Nil(t, cd.pendingTraces)
	})
}

func TestTraceStart_Abort(t *testing.T) {
	tc := idleTraceCollector(t)
	pending := &fakeTraceSink{}
	traceStart{started: tc, pending: pending}.abort()
	assert.False(t, tc.cleanupPending(), "started collector is stopped")
	assert.Equal(t, 1, pending.stopCalls)
}

func TestBuildCFMintDriverWithOpts_TraceCollector(t *testing.T) {
	t.Run("attached on success", func(t *testing.T) {
		tc := idleTraceCollector(t)
		mint := &testCFMintMintDriver{installMintURL: "https://mint.test"}
		d, err := buildCFMintDriverWithOpts("org", mint, nil, "tok", "/bin/fullsend", "proj", 1,
			playbackSetupOpts(), traceStart{started: tc}, t.Logf)
		require.NoError(t, err)
		assert.Same(t, tc, d.(*composedDriver).traces)
		assert.True(t, tc.cleanupPending(), "an attached collector runs until Finalize")
	})
	t.Run("stopped when the mint deploy fails", func(t *testing.T) {
		tc := idleTraceCollector(t)
		pending := &fakeTraceSink{}
		mint := &testCFMintMintDriver{installErr: fmt.Errorf("deploy boom")}
		_, err := buildCFMintDriverWithOpts("org", mint, nil, "tok", "/bin/fullsend", "proj", 1,
			playbackSetupOpts(), traceStart{started: tc, pending: pending}, t.Logf)
		require.Error(t, err)
		assert.False(t, tc.cleanupPending(), "collector must not outlive a failed factory")
		assert.Equal(t, 1, pending.stopCalls)
	})
}

func TestCFMintDeployArgs_TraceCollector(t *testing.T) {
	base := cfmintConfig{pemDir: "/tmp/pems", suiteName: "bt"}

	args := CFMintDeployArgs("bt-abc12345", "bt-mint", base)
	assert.NotContains(t, args, "--otlp-traces-endpoint", "no collector, no tracing flags")
	assert.NotContains(t, args, "--otlp-headers-file")

	traced := base
	traced.traceEndpoint = "https://example-tunnel.trycloudflare.com/v1/traces"
	traced.traceHeadersFile = "/tmp/bt-otlp-headers-1"
	args = CFMintDeployArgs("bt-abc12345", "bt-mint", traced)
	assert.Equal(t, []string{
		"--otlp-traces-endpoint", "https://example-tunnel.trycloudflare.com/v1/traces",
		"--otlp-headers-file", "/tmp/bt-otlp-headers-1",
	}, args[len(args)-4:], "the token travels in a file, never on the command line")
}
