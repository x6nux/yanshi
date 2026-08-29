package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/netpolicy"
	"github.com/x6nux/yanshi/internal/shell"
)

// TestBothLaunchFactoriesPublishTheManagedProxy is the regression guard for
// W-B-16's wiring half.
//
// The two production launch paths share one childLaunchPosture, so their env
// SEMANTICS could not drift — and that is exactly why nothing noticed when
// their INPUTS did. bootstrap set DefaultSecureFactory.ProxyURL and omitted it
// from the SecureLaunchFactory literal, so shell_run went through the managed
// proxy while shell_start, task_shell_start and every other shell v2 tool
// published an empty http_proxy, which a child reads as "no proxy" and answers
// by connecting directly.
//
// No unit test in internal/shell could see it: they all substitute their own
// factory, which is the object that was wrong.
func TestBothLaunchFactoriesPublishTheManagedProxy(t *testing.T) {
	app, err := Build(Options{ConfigPath: w3ConfigFile(t), FakeModel: true})
	require.NoError(t, err)
	defer app.Shutdown(context.Background())

	require.Len(t, app.LaunchProxyURLs, 2,
		"both production launch factories must report a proxy URL")
	for name, url := range app.LaunchProxyURLs {
		require.NotEmpty(t, url,
			"the %s launch factory publishes no proxy URL, so its children egress unfiltered", name)
		require.True(t, strings.HasPrefix(url, "http://127.0.0.1:"),
			"the %s factory publishes %q, which is not a loopback proxy", name, url)
	}
	require.Equal(t, app.LaunchProxyURLs["secproc"], app.LaunchProxyURLs["shell_v2"],
		"the two launch paths point at different proxies")

	// The SOCKS endpoint has exactly one production producer — this wiring —
	// and exactly one production consumer, the ALL_PROXY variable the posture
	// publishes. Drop it here and the SOCKS5 handler is code with no clients:
	// every ALL_PROXY-honouring child goes back to connecting directly, and
	// nothing anywhere reports it. A probe confirmed that deleting the
	// assignment used to leave every test green.
	secure, v2 := productionFactories(t, app)
	host := strings.TrimPrefix(app.LaunchProxyURLs["secproc"], "http://")
	for name, socks := range map[string]string{"secproc": secure.SOCKSURL, "shell_v2": v2.SOCKSURL} {
		require.Equal(t, "socks5h://"+host, socks,
			"the %s launch factory publishes no usable SOCKS endpoint, so ALL_PROXY-honouring "+
				"children bypass the managed proxy entirely", name)
	}
}

// TestAFailedProfileCaptureDoesNotRefuseTheBoot is W-B-21's fail-safe clause
// measured where it matters.
//
// CaptureSnapshot's own test pins that a failure yields the zero Snapshot; this
// pins that bootstrap USES it that way. A probe confirmed the two are
// independent: turning the warning into `return nil, snapErr` left every other
// test green, and the result would be a yanshi that refuses to start because
// the operator named a shell they do not have installed.
func TestAFailedProfileCaptureDoesNotRefuseTheBoot(t *testing.T) {
	dir := t.TempDir()
	skillsDir := filepath.Join(dir, "skills")
	require.NoError(t, os.MkdirAll(skillsDir, 0o755))
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(
		"server:\n  http_addr: 127.0.0.1:0\n"+
			"storage:\n  sqlite_path: \":memory:\"\n"+
			"skills:\n  user_dir: "+skillsDir+"\n"+
			"security:\n  shell:\n    capture_profile: true\n"+
			"    profile_shell: definitely-not-a-shell\n"), 0o644))

	app, err := Build(Options{ConfigPath: path, FakeModel: true})
	require.NoError(t, err, "an uncapturable login shell refused the whole boot")
	defer app.Shutdown(context.Background())

	// And the children still get a working environment: the zero Snapshot's
	// Apply is the identity, so the posture is exactly the un-captured one.
	_, v2 := productionFactories(t, app)
	require.True(t, v2.Snapshot.Empty(),
		"a failed capture produced a non-empty snapshot, so children are being handed "+
			"a partially-read environment")
}

// productionFactories reads both assembled launch factories back off the App.
func productionFactories(t *testing.T, app *App) (shell.DefaultSecureFactory, shell.SecureLaunchFactory) {
	t.Helper()
	secure, ok := app.SecureFactory.(shell.DefaultSecureFactory)
	require.True(t, ok, "the orchestrator is not running the production secure factory")
	v2, ok := app.ShellManager.Factory().(shell.SecureLaunchFactory)
	require.True(t, ok, "the shell manager is not running the production factory")
	return secure, v2
}

// TestManagedProxyIsAskableFromTheAssembledServer pins the other half: the
// proxy has to be able to reach a human, and the object that can is built
// several hundred lines after the proxy is.
//
// It asserts on the SERVER's own approver behaviour rather than on a field,
// because a SetApprover call that was never made and one made with a value
// that always refuses are the same wire-up bug from the proxy's side.
func TestManagedProxyIsAskableFromTheAssembledServer(t *testing.T) {
	app, err := Build(Options{ConfigPath: w3ConfigFile(t), FakeModel: true})
	require.NoError(t, err)
	defer app.Shutdown(context.Background())

	require.NotNil(t, app.NetProxy, "no managed proxy was started")
	require.True(t, app.NetProxy.HasApprover(),
		"the managed proxy has nobody to ask, so every host outside security.network.allow "+
			"is refused with no way for the operator to say yes")
}

// TestInspectionAndCaptureWireUpWhenAsked is the opted-IN shape. It is a
// separate test rather than a flag on the others because both features have a
// side effect on the operator's machine — a generated CA under ~/.yanshi/tls
// and one execution of their login shell — so it runs against a temporary HOME
// and never touches the real one.
//
// What it pins is the wiring, not the behaviour: the CA path reaches BOTH
// launch factories (a child that is not told to trust the root fails every
// handshake), the method table reaches the policy, and the concurrency cap
// reaches the manager. Each of those is a value that has to survive a copy
// through three structs to matter.
func TestInspectionAndCaptureWireUpWhenAsked(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	dir := t.TempDir()
	skillsDir := filepath.Join(dir, "skills")
	require.NoError(t, os.MkdirAll(skillsDir, 0o755))
	path := filepath.Join(dir, "config.yaml")
	body := "server:\n  http_addr: 127.0.0.1:0\n" +
		"storage:\n  sqlite_path: \":memory:\"\n" +
		"skills:\n  user_dir: " + skillsDir + "\n" +
		"security:\n" +
		"  network:\n" +
		"    default: deny\n" +
		"    inspect_https: true\n" +
		"    methods:\n" +
		"      - host: api.test\n        methods: [GET]\n        action: allow\n" +
		"      - host: api.test\n        action: deny\n" +
		"  shell:\n    max_concurrent: 3\n"
	if runtime.GOOS != "windows" {
		// The POSIX capture argv is `sh -l -c env`; the windows one needs
		// powershell, which a CI image may not have on PATH under that exact
		// name. Capture failure is non-fatal by design, so leaving it off
		// there costs this test nothing it can assert.
		body += "    capture_profile: true\n    profile_shell: sh\n"
	}
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))

	app, err := Build(Options{ConfigPath: path, FakeModel: true})
	require.NoError(t, err)
	defer app.Shutdown(context.Background())

	require.Len(t, app.NetworkPolicy.Methods, 2, "the method table did not reach the policy")
	require.True(t, app.NetworkPolicy.Methods[0].Allow, "the first rule's verdict was lost")
	require.False(t, app.NetworkPolicy.Methods[1].Allow, "the second rule's verdict was lost")
	require.Equal(t, []string{"GET"}, app.NetworkPolicy.Methods[0].Methods)

	// The generated root must exist and be private, and both launch factories
	// must point their children at it.
	caPath := filepath.Join(home, ".yanshi", "tls", "ca.pem")
	require.FileExists(t, caPath)
	if runtime.GOOS != "windows" {
		info, statErr := os.Stat(filepath.Join(home, ".yanshi", "tls", "ca-key.pem"))
		require.NoError(t, statErr)
		require.Zero(t, info.Mode().Perm()&0o077, "the generated CA key is group/other readable")
	}
	for name, env := range childTrustEnvs(t, app) {
		require.Contains(t, env, "SSL_CERT_FILE="+caPath,
			"the %s launch factory does not tell its children to trust the inspection root, "+
				"so every HTTPS request they make fails the handshake", name)
	}
}

// childTrustEnvs renders the certificate variables each production launch
// factory would hand a child, read back from the ASSEMBLED objects.
//
// Both are checked because the CA path travels the same route the proxy URL
// did, through two separate struct literals — and that route is exactly where
// shell v2 lost its proxy URL while secproc kept it.
func childTrustEnvs(t *testing.T, app *App) map[string]string {
	t.Helper()
	secure, v2 := productionFactories(t, app)
	return map[string]string{
		"shell_v2": strings.Join(netpolicy.CAEnv(v2.CAFile), "\n"),
		"secproc":  strings.Join(netpolicy.CAEnv(secure.CAFile), "\n"),
	}
}

// The fail-closed behaviour of that approver — "no client connected means no"
// — is pinned where the approver lives, in
// internal/api/http::TestApproveEgressRefusesWithNoConnectedClient. Asserting
// it here as well would need a type assertion back to *apihttp.Server, which
// the C4 plan forbids because the handler is wrapped by middleware.

// TestInspectionIsOffByDefault pins ADR-0023's opt-in decision at the
// composition root: with no security.network.inspect_https in the config, no
// certificate variable reaches a child and CONNECT stays the blind tunnel
// ADR-0014 specified.
func TestInspectionIsOffByDefault(t *testing.T) {
	app, err := Build(Options{ConfigPath: w3ConfigFile(t), FakeModel: true})
	require.NoError(t, err)
	defer app.Shutdown(context.Background())

	// The CA directory is the observable artefact: LoadOrCreateCA creates it
	// on first use, so its absence means inspection never started.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory to check: %v", err)
	}
	caKey := filepath.Join(home, ".yanshi", "tls", "ca-key.pem")
	if _, err := os.Stat(caKey); err == nil {
		t.Skipf("%s already exists from an operator who opted in; this test cannot "+
			"distinguish that from a default-on regression", caKey)
	}
	require.Empty(t, app.NetworkPolicy.Methods,
		"the default configuration carries method rules it cannot enforce")
}
