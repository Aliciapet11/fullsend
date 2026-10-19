package gcf

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testTraceEndpoint = "https://collector.example/v1/traces"
	// testHeaderValue is a fake collector credential that must never
	// appear in generated function source.
	testHeaderValue = "Bearer header-value-never-in-source"
)

func testTraceHeaders() []byte {
	return []byte("authorization=" + testHeaderValue)
}

// zipFiles returns the files in a source zip by name.
func zipFiles(t *testing.T, data []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)
	out := make(map[string]string, len(zr.File))
	for _, f := range zr.File {
		rc, err := f.Open()
		require.NoError(t, err)
		content, err := io.ReadAll(rc)
		rc.Close()
		require.NoError(t, err)
		out[f.Name] = string(content)
	}
	return out
}

func activeFunction(hash string) *FunctionInfo {
	return &FunctionInfo{
		Name:  "projects/my-project/locations/us-central1/functions/fullsend-mint",
		State: "ACTIVE",
		URI:   "https://fullsend-mint-abc123.run.app",
		EnvVars: map[string]string{
			"GCP_PROJECT_NUMBER":     "123456789",
			"WIF_POOL_NAME":          "fullsend-pool",
			"WIF_PROVIDER_NAME":      "github-oidc",
			"ALLOWED_ROLES":          "coder",
			"ROLE_APP_IDS":           `{"coder":"12345"}`,
			"FULLSEND_SOURCE_HASH":   hash,
			"ALLOWED_WORKFLOW_FILES": "*",
		},
	}
}

func TestBundleEmbeddedMintSource_StampsTracing(t *testing.T) {
	stamp := TracingStamp{Endpoint: testTraceEndpoint, Deployment: "fullsend-mint/us-central1", HeadersSecret: TraceHeadersSecretID}
	data, err := bundleEmbeddedMintSource("1.0.0", "abc", StatusGitHubAuth{}, stamp)
	require.NoError(t, err)

	src, ok := zipFiles(t, data)["trace_consts.go"]
	require.True(t, ok, "trace_consts.go missing from bundle")
	assert.Contains(t, src, "package function")
	assert.Contains(t, src, `otlpTracesEndpoint = "https://collector.example/v1/traces"`)
	assert.Contains(t, src, `traceDeployment    = "fullsend-mint/us-central1"`)
	assert.Contains(t, src, `otlpHeadersSecret  = "fullsend-mint-otlp-headers"`)
}

func TestBundleEmbeddedMintSource_TracingDisabledByDefault(t *testing.T) {
	data, err := bundleEmbeddedMintSource("", "", StatusGitHubAuth{}, TracingStamp{})
	require.NoError(t, err)
	src := zipFiles(t, data)["trace_consts.go"]
	assert.Contains(t, src, `otlpTracesEndpoint = ""`)
}

func TestBundleFunctionSource_StampsTracingOverDiskCopy(t *testing.T) {
	srcDir := fakeFunctionSourceDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "trace_consts.go"), []byte("package function\n\nvar otlpTracesEndpoint = \"https://stale.example/v1/traces\"\n"), 0o644))

	data, err := bundleFunctionSource(srcDir, "", "", StatusGitHubAuth{}, TracingStamp{Endpoint: testTraceEndpoint, Deployment: "d"})
	require.NoError(t, err)

	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)
	count := 0
	for _, f := range zr.File {
		if f.Name == "trace_consts.go" {
			count++
		}
	}
	assert.Equal(t, 1, count, "trace_consts.go must appear exactly once")
	src := zipFiles(t, data)["trace_consts.go"]
	assert.Contains(t, src, testTraceEndpoint)
	assert.NotContains(t, src, "stale.example")
}

func TestBundle_TracingChangesSourceHash(t *testing.T) {
	plain, err := bundleEmbeddedMintSource("", "", StatusGitHubAuth{}, TracingStamp{})
	require.NoError(t, err)
	traced, err := bundleEmbeddedMintSource("", "", StatusGitHubAuth{}, TracingStamp{Endpoint: testTraceEndpoint, Deployment: "d"})
	require.NoError(t, err)
	assert.NotEqual(t, sha256Hex(plain), sha256Hex(traced))
}

func TestTraceDeploymentName(t *testing.T) {
	assert.Equal(t, "fullsend-mint/europe-west4", TraceDeploymentName("europe-west4"))
	assert.Equal(t, "fullsend-mint/us-central1", TraceDeploymentName(""))
}

func TestProvisioner_Provision_TracingNewMint(t *testing.T) {
	fake := newFakeGCFClient()
	fake.secrets = map[string]bool{}
	fake.secretData = map[string][]byte{}
	fake.functionInfoAfterCreate = &FunctionInfo{URI: "https://mint.run.app"}
	headers := testTraceHeaders()

	p := newTestProvisioner(Config{
		ProjectID:         "my-project",
		Region:            "europe-west4",
		GitHubOrgs:        []string{PlaceholderOrg},
		AgentPEMs:         singleRolePEMs(),
		AgentAppIDs:       singleRoleAppIDs(),
		FunctionSourceDir: fakeFunctionSourceDir(t),
		Tracing:           TracingConfig{Endpoint: testTraceEndpoint, Headers: headers},
	}, fake)

	_, err := p.Provision(context.Background())
	require.NoError(t, err)

	assert.True(t, fake.secrets[TraceHeadersSecretID], "headers secret not created")
	assert.Equal(t, string(testTraceHeaders()), string(fake.secretData[TraceHeadersSecretID]))
	assert.Contains(t, fake.calls, "CreateFunction")

	files := zipFiles(t, fake.lastUploadedSource)
	assert.Contains(t, files["trace_consts.go"], testTraceEndpoint)
	assert.Contains(t, files["trace_consts.go"], `"fullsend-mint/europe-west4"`)
	assert.Contains(t, files["trace_consts.go"], `"`+TraceHeadersSecretID+`"`)
	for name, content := range files {
		assert.NotContains(t, content, "header-value-never-in-source", "collector header value leaked into %s", name)
	}
	assert.Equal(t, make([]byte, len(headers)), headers, "headers not zeroed after Provision")
}

func TestProvisioner_Provision_TracingNoHeaders(t *testing.T) {
	fake := newFakeGCFClient()
	fake.secrets = map[string]bool{}
	fake.functionInfoAfterCreate = &FunctionInfo{URI: "https://mint.run.app"}

	p := newTestProvisioner(Config{
		ProjectID:         "my-project",
		GitHubOrgs:        []string{PlaceholderOrg},
		AgentAppIDs:       singleRoleAppIDs(),
		FunctionSourceDir: fakeFunctionSourceDir(t),
		Tracing:           TracingConfig{Endpoint: testTraceEndpoint},
	}, fake)
	fake.secrets["fullsend-coder-app-pem"] = true

	_, err := p.Provision(context.Background())
	require.NoError(t, err)

	assert.False(t, fake.secrets[TraceHeadersSecretID], "no headers secret expected")
	src := zipFiles(t, fake.lastUploadedSource)["trace_consts.go"]
	assert.Contains(t, src, testTraceEndpoint)
	assert.Contains(t, src, `otlpHeadersSecret  = ""`)
}

// Rotating headers adds a secret version and redeploys even though the
// generated source is unchanged, so new instances read the new headers.
func TestProvisioner_Provision_TracingHeaderRotationRedeploys(t *testing.T) {
	srcDir := fakeFunctionSourceDir(t)
	stamp := TracingStamp{Endpoint: testTraceEndpoint, Deployment: TraceDeploymentName(""), HeadersSecret: TraceHeadersSecretID}
	sourceZip, err := bundleFunctionSource(srcDir, "", "", StatusGitHubAuth{}, stamp)
	require.NoError(t, err)

	fake := newFakeGCFClient()
	fake.functionInfo = activeFunction(sha256Hex(sourceZip))
	fake.secrets = map[string]bool{TraceHeadersSecretID: true, "fullsend-coder-app-pem": true}
	fake.secretData = map[string][]byte{TraceHeadersSecretID: []byte("authorization=old")}

	p := newTestProvisioner(Config{
		ProjectID:         "my-project",
		GitHubOrgs:        []string{PlaceholderOrg},
		AgentAppIDs:       singleRoleAppIDs(),
		FunctionSourceDir: srcDir,
		Tracing:           TracingConfig{Endpoint: testTraceEndpoint, Headers: testTraceHeaders()},
	}, fake)

	_, err = p.Provision(context.Background())
	require.NoError(t, err)

	assert.Equal(t, string(testTraceHeaders()), string(fake.secretData[TraceHeadersSecretID]))
	assert.Contains(t, fake.calls, "UpdateFunction")
}

// Re-running with the same headers and source writes no secret version
// and does not redeploy.
func TestProvisioner_Provision_TracingUnchangedSkipsRedeploy(t *testing.T) {
	srcDir := fakeFunctionSourceDir(t)
	stamp := TracingStamp{Endpoint: testTraceEndpoint, Deployment: TraceDeploymentName(""), HeadersSecret: TraceHeadersSecretID}
	sourceZip, err := bundleFunctionSource(srcDir, "", "", StatusGitHubAuth{}, stamp)
	require.NoError(t, err)

	fake := newFakeGCFClient()
	fake.functionInfo = activeFunction(sha256Hex(sourceZip))
	fake.secrets = map[string]bool{TraceHeadersSecretID: true, "fullsend-coder-app-pem": true}
	fake.secretData = map[string][]byte{TraceHeadersSecretID: testTraceHeaders()}

	p := newTestProvisioner(Config{
		ProjectID:         "my-project",
		GitHubOrgs:        []string{PlaceholderOrg},
		AgentAppIDs:       singleRoleAppIDs(),
		FunctionSourceDir: srcDir,
		Tracing:           TracingConfig{Endpoint: testTraceEndpoint, Headers: testTraceHeaders()},
	}, fake)

	_, err = p.Provision(context.Background())
	require.NoError(t, err)

	assert.NotContains(t, fake.calls, "AddSecretVersion")
	assert.Contains(t, fake.calls, "SetSecretIAMBinding")
	assert.NotContains(t, fake.calls, "UpdateFunction")
	assert.NotContains(t, fake.calls, "UploadFunctionSource")
}

// Enabling tracing on an existing untraced mint changes the generated
// source and redeploys it.
func TestProvisioner_Provision_EnablingTracingRedeploys(t *testing.T) {
	srcDir := fakeFunctionSourceDir(t)
	sourceZip, err := bundleFunctionSource(srcDir, "", "", StatusGitHubAuth{}, TracingStamp{})
	require.NoError(t, err)

	fake := newFakeGCFClient()
	fake.functionInfo = activeFunction(sha256Hex(sourceZip))
	fake.secrets = map[string]bool{"fullsend-coder-app-pem": true}

	p := newTestProvisioner(Config{
		ProjectID:         "my-project",
		GitHubOrgs:        []string{PlaceholderOrg},
		AgentAppIDs:       singleRoleAppIDs(),
		FunctionSourceDir: srcDir,
		Tracing:           TracingConfig{Endpoint: testTraceEndpoint},
	}, fake)

	_, err = p.Provision(context.Background())
	require.NoError(t, err)
	assert.Contains(t, fake.calls, "UpdateFunction")
	assert.Contains(t, zipFiles(t, fake.lastUploadedSource)["trace_consts.go"], testTraceEndpoint)
}

func TestProvisioner_Provision_TracingErrors(t *testing.T) {
	base := func() Config {
		return Config{
			ProjectID:         "my-project",
			GitHubOrgs:        []string{PlaceholderOrg},
			AgentAppIDs:       singleRoleAppIDs(),
			FunctionSourceDir: fakeFunctionSourceDir(t),
		}
	}
	tests := []struct {
		name    string
		mutate  func(*Config)
		fakeErr map[string]error
		wantErr string
	}{
		{"plain http endpoint", func(c *Config) { c.Tracing.Endpoint = "http://collector.example/v1/traces" }, nil, "must use https"},
		{"credentials in URL", func(c *Config) { c.Tracing.Endpoint = "https://u:p@collector.example/v1/traces" }, nil, "credentials"},
		{"headers without endpoint", func(c *Config) { c.Tracing.Headers = testTraceHeaders() }, nil, "require an OTLP traces endpoint"},
		{"malformed headers", func(c *Config) {
			c.Tracing = TracingConfig{Endpoint: testTraceEndpoint, Headers: []byte("not-a-pair")}
		}, nil, "invalid collector headers"},
		{"skip deploy", func(c *Config) {
			c.Tracing.Endpoint = testTraceEndpoint
			c.DeployMode = DeploySkip
		}, nil, "--skip-mint-deploy"},
		{"existing mint URL", func(c *Config) {
			c.Tracing.Endpoint = testTraceEndpoint
			c.MintURL = "https://mint.run.app"
		}, nil, "existing MintURL"},
		{"secret check fails", func(c *Config) {
			c.Tracing = TracingConfig{Endpoint: testTraceEndpoint, Headers: testTraceHeaders()}
		}, map[string]error{"GetSecret": errors.New("boom")}, "checking secret fullsend-mint-otlp-headers"},
		{"secret create fails", func(c *Config) {
			c.Tracing = TracingConfig{Endpoint: testTraceEndpoint, Headers: testTraceHeaders()}
		}, map[string]error{"CreateSecret": errors.New("boom")}, "creating secret fullsend-mint-otlp-headers"},
		{"add version fails", func(c *Config) {
			c.Tracing = TracingConfig{Endpoint: testTraceEndpoint, Headers: testTraceHeaders()}
		}, map[string]error{"AddSecretVersion": errors.New("boom")}, "adding secret version for fullsend-mint-otlp-headers"},
		{"IAM binding fails", func(c *Config) {
			c.Tracing = TracingConfig{Endpoint: testTraceEndpoint, Headers: testTraceHeaders()}
		}, map[string]error{"SetSecretIAMBinding": errors.New("boom")}, "granting secret access for fullsend-mint-otlp-headers"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeGCFClient()
			fake.secrets = map[string]bool{}
			for k, v := range tt.fakeErr {
				fake.errs[k] = v
			}
			cfg := base()
			tt.mutate(&cfg)
			_, err := newTestProvisioner(cfg, fake).Provision(context.Background())
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.NotContains(t, err.Error(), "header-value-never-in-source")
			assert.NotContains(t, fake.calls, "CreateFunction")
		})
	}
}
