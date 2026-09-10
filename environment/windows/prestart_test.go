package windows

import (
	"reflect"
	"strings"
	"testing"
)

const (
	testExe = `C:\srv\data\steamcmd\steamcmd.exe`
	testDir = `C:\srv\data`
)

func TestSteamcmdUpdate(t *testing.T) {
	cases := []struct {
		name   string
		vars   map[string]string
		want   []string
		reason string
	}{
		{
			name:   "auto update unset",
			vars:   map[string]string{"SRCDS_APPID": "740"},
			reason: "AUTO_UPDATE",
		},
		{
			name:   "auto update off",
			vars:   map[string]string{"AUTO_UPDATE": "0", "SRCDS_APPID": "740"},
			reason: "AUTO_UPDATE",
		},
		{
			name:   "no app id",
			vars:   map[string]string{"AUTO_UPDATE": "1"},
			reason: "SRCDS_APPID",
		},
		{
			name: "anonymous",
			vars: map[string]string{"AUTO_UPDATE": "1", "SRCDS_APPID": "740"},
			want: []string{testExe, "+force_install_dir", testDir, "+login", "anonymous",
				"+app_update", "740", "+quit"},
		},
		{
			name: "true spelling and steam app id fallback",
			vars: map[string]string{"AUTO_UPDATE": "True", "STEAM_APPID": "896660"},
			want: []string{testExe, "+force_install_dir", testDir, "+login", "anonymous",
				"+app_update", "896660", "+quit"},
		},
		{
			name: "credentials, guard code, beta and validate",
			vars: map[string]string{
				"AUTO_UPDATE": "yes", "SRCDS_APPID": "380870",
				"STEAM_USER": "someone", "STEAM_PASS": "hunter2", "STEAM_AUTH": "ABCDE",
				"SRCDS_BETAID": "experimental", "SRCDS_BETAPASS": "beta-secret",
				"VALIDATE": "1",
			},
			want: []string{testExe, "+force_install_dir", testDir, "+login", "someone", "hunter2", "ABCDE",
				"+app_update", "380870", "-beta", "experimental", "-betapassword", "beta-secret",
				"validate", "+quit"},
		},
		{
			name: "install flags are split like a command line",
			vars: map[string]string{"AUTO_UPDATE": "1", "SRCDS_APPID": "740",
				"INSTALL_FLAGS": `-beta "some branch"`},
			want: []string{testExe, "+force_install_dir", testDir, "+login", "anonymous",
				"+app_update", "740", "-beta", "some branch", "+quit"},
		},
		{
			name: "explicit anonymous user ignores a stray password",
			vars: map[string]string{"AUTO_UPDATE": "1", "SRCDS_APPID": "740",
				"STEAM_USER": "Anonymous", "STEAM_PASS": "x"},
			want: []string{testExe, "+force_install_dir", testDir, "+login", "anonymous",
				"+app_update", "740", "+quit"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := steamcmdUpdate(testExe, testDir, tc.vars)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("got %v, want no command", got)
				}
				if !strings.Contains(reason, tc.reason) {
					t.Fatalf("reason %q does not mention %q", reason, tc.reason)
				}
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got  %v\nwant %v", got, tc.want)
			}
		})
	}
}

func TestRedactSteamSecrets(t *testing.T) {
	in := []string{testExe, "+login", "someone", "hunter2", "ABCDE",
		"+app_update", "1", "-betapassword", "beta-secret", "+quit"}
	got := strings.Join(redactSteamSecrets(in), " ")
	for _, secret := range []string{"hunter2", "beta-secret"} {
		if strings.Contains(got, secret) {
			t.Errorf("redacted line still contains %q: %s", secret, got)
		}
	}
	if !strings.Contains(got, "someone") || !strings.Contains(got, "ABCDE") {
		t.Errorf("redaction removed more than the passwords: %s", got)
	}
	if in[3] != "hunter2" {
		t.Error("redaction modified the original argv")
	}

	anon := redactSteamSecrets([]string{testExe, "+login", "anonymous", "+app_update", "1", "+quit"})
	if anon[3] != "+app_update" {
		t.Errorf("anonymous login had an argument masked: %v", anon)
	}
}
