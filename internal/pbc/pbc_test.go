package pbc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/incus-pbs/incus-pbs/internal/execx"
)

type fakeRunner struct {
	calls []execx.Cmd
	res   execx.Result
	err   error
}

func (f *fakeRunner) Run(_ context.Context, c execx.Cmd) (execx.Result, error) {
	f.calls = append(f.calls, c)
	return f.res, f.err
}

func secretFile(t *testing.T, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, []byte("s3cr3t-token-value\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBackupArgs(t *testing.T) {
	args, err := BackupArgs(BackupOptions{
		Namespace: "incus", BackupType: "host", BackupID: "h1", KeyFile: "/k.json",
		ChangeDetection: "metadata",
		Archives: []Archive{
			{Name: "ct_default_c1.pxar", Source: "/stage/mnt/ct_default_c1"},
			{Name: "vm_default_v1.img", Source: "/dev/zvol/tank/x"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"backup", "ct_default_c1.pxar:/stage/mnt/ct_default_c1", "vm_default_v1.img:/dev/zvol/tank/x",
		"--backup-type", "host", "--backup-id", "h1", "--ns", "incus", "--keyfile", "/k.json",
		"--change-detection-mode", "metadata"}
	if !slices.Equal(args, want) {
		t.Fatalf("args = %q\nwant %q", args, want)
	}
}

func TestBackupArgsNoKeyfileDisablesCrypt(t *testing.T) {
	args, err := BackupArgs(BackupOptions{BackupType: "host", BackupID: "h", Archives: []Archive{{Name: "a.pxar", Source: "/a"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(args, "--crypt-mode") || slices.Contains(args, "--ns") {
		t.Fatalf("unexpected args %q", args)
	}
}

func TestBackupArgsRejectsBadInput(t *testing.T) {
	for name, o := range map[string]BackupOptions{
		"no archives":     {BackupType: "host", BackupID: "h"},
		"no id":           {BackupType: "host", Archives: []Archive{{Name: "a.pxar", Source: "/a"}}},
		"bad extension":   {BackupType: "host", BackupID: "h", Archives: []Archive{{Name: "a.tar", Source: "/a"}}},
		"injection name":  {BackupType: "host", BackupID: "h", Archives: []Archive{{Name: "a;rm -rf.pxar", Source: "/a"}}},
		"colon in name":   {BackupType: "host", BackupID: "h", Archives: []Archive{{Name: "a:b.pxar", Source: "/a"}}},
		"relative source": {BackupType: "host", BackupID: "h", Archives: []Archive{{Name: "a.pxar", Source: "a"}}},
		"duplicate":       {BackupType: "host", BackupID: "h", Archives: []Archive{{Name: "a.pxar", Source: "/a"}, {Name: "a.pxar", Source: "/b"}}},
	} {
		if _, err := BackupArgs(o); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestBackupNeverPutsSecretsOnArgv(t *testing.T) {
	pw := secretFile(t, 0o600)
	f := &fakeRunner{}
	c := &Client{Repository: "u@pbs!t@host:ds", PasswordFile: pw, Fingerprint: "aa:bb", X: f}
	_, err := c.Backup(context.Background(), BackupOptions{BackupType: "host", BackupID: "h",
		Archives: []Archive{{Name: "a.pxar", Source: "/a"}}})
	if err != nil {
		t.Fatal(err)
	}
	call := f.calls[0]
	joined := strings.Join(call.Args, " ")
	if strings.Contains(joined, "s3cr3t") || strings.Contains(joined, "repository") || strings.Contains(joined, pw) {
		t.Fatalf("secret material on argv: %q", call.Args)
	}
	if !slices.Contains(call.Env, "PBS_PASSWORD_FILE="+pw) || !slices.Contains(call.Env, "PBS_REPOSITORY=u@pbs!t@host:ds") {
		t.Fatalf("env missing settings: %q", call.Env)
	}
	for _, e := range call.Env {
		if strings.Contains(e, "s3cr3t") {
			t.Fatalf("secret value in env: %q", e)
		}
	}
}

func TestBackupMissingOrInsecureCredentials(t *testing.T) {
	f := &fakeRunner{}
	opts := BackupOptions{BackupType: "host", BackupID: "h", Archives: []Archive{{Name: "a.pxar", Source: "/a"}}}
	cases := map[string]*Client{
		"no repository":    {PasswordFile: secretFile(t, 0o600), X: f},
		"no password file": {Repository: "r", X: f},
		"missing file":     {Repository: "r", PasswordFile: "/nonexistent/secret", X: f},
		"world readable":   {Repository: "r", PasswordFile: secretFile(t, 0o644), X: f},
	}
	for name, c := range cases {
		if _, err := c.Backup(context.Background(), opts); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("client must not be executed with invalid credentials, got %d calls", len(f.calls))
	}
	insecureKey := &Client{Repository: "r", PasswordFile: secretFile(t, 0o600), X: f}
	o := opts
	o.KeyFile = secretFile(t, 0o640)
	if _, err := insecureKey.Backup(context.Background(), o); err == nil {
		t.Error("group-readable keyfile must be rejected")
	}
}

const sampleOutput = `Starting backup: [incus-host]:host/incus-host/2026-11-24T08:00:00Z
Client name: incus-host
Starting backup protocol: Mon Nov 24 08:00:00 2026
Upload directory '/var/cache/incus-pbs/mnt/ct_default_c1' to 'backup@pbs!incus@pbs.example.net:8007:store' as ct_default_c1.mpxar.didx
ct_default_c1.mpxar: had to backup 151.2 KiB of 2.1 MiB (compressed 30.4 KiB) in 1.20 s (average 126.0 KiB/s)
ct_default_c1.mpxar: backup was done incrementally, reused 1.95 MiB (92.9%)
ct_default_c1.ppxar: had to backup 0 B of 412.5 MiB (compressed 0 B) in 1.21 s (average 0 B/s)
ct_default_c1.ppxar: backup was done incrementally, reused 412.5 MiB (100.0%)
vm_default_v1.img: had to backup 24 MiB of 8 GiB (compressed 6.5 MiB) in 35.10s (average 700.2 KiB/s)
vm_default_v1.img: backup was done incrementally, reused 7.98 GiB (99.7%)
Duration: 38.40s
End Time: Mon Nov 24 08:00:38 2026
`

func TestParseStats(t *testing.T) {
	s := ParseStats(sampleOutput)
	if len(s) != 3 {
		t.Fatalf("got %d stats: %+v", len(s), s)
	}
	if s[0].Archive != "ct_default_c1.mpxar" || s[0].UploadedBytes != 154828 || s[0].ReusedPercent != 92.9 {
		t.Errorf("mpxar: %+v", s[0])
	}
	if s[1].UploadedBytes != 0 || s[1].ReusedPercent != 100 {
		t.Errorf("ppxar: %+v", s[1])
	}
	if s[2].TotalBytes != 8<<30 || s[2].UploadedBytes != 24<<20 || s[2].Seconds != 35.1 {
		t.Errorf("img: %+v", s[2])
	}
	if got := parseSnapshot(sampleOutput); got != "host/incus-host/2026-11-24T08:00:00Z" {
		t.Errorf("snapshot = %q", got)
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"0 B": 0, "512 B": 512, "1 KiB": 1024, "1.5 MiB": 1572864, "2GiB": 2 << 30, "1 KB": 1000} {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "GiB", "12 parsecs"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q): expected error", bad)
		}
	}
}

func TestBackupFailurePropagatesAndKeepsOutput(t *testing.T) {
	f := &fakeRunner{
		res: execx.Result{Stderr: "Error: connection reset by peer", ExitCode: 255},
		err: &execx.ExitError{Name: "proxmox-backup-client", ExitCode: 255},
	}
	c := &Client{Repository: "r", PasswordFile: secretFile(t, 0o600), X: f}
	br, err := c.Backup(context.Background(), BackupOptions{BackupType: "host", BackupID: "h", Archives: []Archive{{Name: "a.pxar", Source: "/a"}}})
	var ee *execx.ExitError
	if !errors.As(err, &ee) || br.ExitCode != 255 || !strings.Contains(br.Output, "connection reset") {
		t.Fatalf("err=%v result=%+v", err, br)
	}
}
