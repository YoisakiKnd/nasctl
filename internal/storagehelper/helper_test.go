package storagehelper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YoisakiKnd/nasctl/internal/storage"
)

type call struct {
	stdin string
	argv  []string
}

type fixture struct {
	*Helper
	dir   string
	calls []call
	fail  map[string]bool
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	me, err := user.Current()
	if err != nil {
		t.Skip("no current user")
	}
	f := &fixture{dir: dir, fail: map[string]bool{}}
	data := filepath.Join(dir, "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "samba"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "samba", "smb.conf"), []byte("[global]\n   workgroup = WORKGROUP\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mountinfo"), []byte("22 1 8:2 / / rw,relatime - ext4 /dev/sda2 rw\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.Helper = &Helper{
		MountBase:    filepath.Join(dir, "media"),
		Fstab:        filepath.Join(dir, "fstab"),
		SambaConf:    filepath.Join(dir, "samba", "smb.conf"),
		SambaInclude: filepath.Join(dir, "samba", "nasctl-shares.conf"),
		StateFile:    filepath.Join(dir, "state", "storage.json"),
		CredDir:      filepath.Join(dir, "credentials"),
		MountInfo:    filepath.Join(dir, "mountinfo"),
		ServiceUser:  me.Username,
		ShareUser:    me.Username,
		Bases:        func() []string { return []string{data} },
		Inventory: func(context.Context) ([]storage.Disk, error) {
			return []storage.Disk{
				{Name: "sda", Path: "/dev/sda", System: true, Partitions: []storage.Partition{{Name: "sda2", Path: "/dev/sda2", FSType: "ext4", System: true, Mountpoints: []string{"/"}}}},
				{Name: "sdb", Path: "/dev/sdb", Serial: "WD-123", Transport: "usb", Partitions: []storage.Partition{
					{Name: "sdb1", Path: "/dev/sdb1", FSType: "ext4", Label: "Backup Disk", UUID: "1111-2222"},
					{Name: "sdb2", Path: "/dev/sdb2", FSType: "zfs_member"},
					{Name: "sdb3", Path: "/dev/sdb3", FSType: "exfat", Mountpoints: []string{"/mnt/x"}},
				}},
			}, nil
		},
		LookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
	}
	f.Run = func(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
		f.calls = append(f.calls, call{string(stdin), append([]string{name}, args...)})
		if f.fail[name] {
			return []byte(name + " said no"), errors.New("exit status 1")
		}
		return nil, nil
	}
	return f
}

func refusal(t *testing.T, err error, want string) {
	t.Helper()
	var f *fail
	if !errors.As(err, &f) || !strings.Contains(f.msg, want) {
		t.Fatalf("got %v, want refusal containing %q", err, want)
	}
}

func TestMountRefusesWhatItMustNotTouch(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	refusal(t, f.mount(ctx, "/dev/sda2", "root", false), "系统盘")
	refusal(t, f.mount(ctx, "/dev/sdz1", "x", false), "找不到")
	refusal(t, f.mount(ctx, "/dev/sdb3", "x", false), "已经挂载")
	refusal(t, f.mount(ctx, "/dev/sdb2", "x", false), "不支持")
	for _, name := range []string{"../etc", "a/b", ".hidden", "with space", strings.Repeat("a", 33)} {
		refusal(t, f.mount(ctx, "/dev/sdb1", name, false), "名称")
	}
	if len(f.calls) != 0 {
		t.Fatalf("commands ran for refused requests: %v", f.calls)
	}
}

func TestMountPersistsUnderManagedBlock(t *testing.T) {
	f := newFixture(t)
	original := "# static\nUUID=abc / ext4 defaults 0 1\n"
	if err := os.WriteFile(f.Fstab, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.mount(context.Background(), "/dev/sdb1", "", true); err != nil {
		t.Fatal(err)
	}
	mp := filepath.Join(f.MountBase, "BackupDisk")
	want := []string{"mount", "-t", "ext4", "-o", "noatime,nosuid,nodev", "/dev/sdb1", mp}
	if len(f.calls) != 1 || strings.Join(f.calls[0].argv, " ") != strings.Join(want, " ") {
		t.Fatalf("calls = %v", f.calls)
	}
	b, _ := os.ReadFile(f.Fstab)
	text := string(b)
	if !strings.HasPrefix(text, original) || !strings.Contains(text, "UUID=1111-2222 "+mp+" ext4 noatime,nosuid,nodev,nofail,x-systemd.device-timeout=10s 0 2") {
		t.Fatalf("fstab:\n%s", text)
	}
	if backup, _ := os.ReadFile(f.Fstab + ".nasctl-backup"); string(backup) != original {
		t.Fatalf("backup = %q", backup)
	}
	st, err := f.status()
	if err != nil || len(st.Persisted) != 1 || st.Persisted[0] != mp {
		t.Fatalf("status = %+v, %v", st, err)
	}
	// Forgetting it restores the file exactly.
	if err := f.setFstab(mp, ""); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(f.Fstab); string(b) != original {
		t.Fatalf("after forget:\n%s", b)
	}
}

func TestFstabKeepsLinesAfterTheBlock(t *testing.T) {
	f := newFixture(t)
	text := "a\n" + fstabBegin + "\nUUID=1 /m/b ext4 x 0 0\n" + fstabEnd + "\nz\n"
	if err := os.WriteFile(f.Fstab, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.setFstab("/m/a", "UUID=2 /m/a ext4 x 0 0"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f.Fstab)
	want := "a\n" + fstabBegin + "\nUUID=2 /m/a ext4 x 0 0\nUUID=1 /m/b ext4 x 0 0\n" + fstabEnd + "\nz\n"
	if string(b) != want {
		t.Fatalf("got\n%s\nwant\n%s", b, want)
	}
}

func TestMountRefusesOccupiedName(t *testing.T) {
	f := newFixture(t)
	mp := filepath.Join(f.MountBase, "taken")
	if err := os.MkdirAll(mp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mp, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	refusal(t, f.mount(context.Background(), "/dev/sdb1", "taken", false), "已有文件")
	f.fail["mount"] = true
	err := f.mount(context.Background(), "/dev/sdb1", "fresh", false)
	if err == nil || !strings.Contains(err.(*fail).detail, "mount said no") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.MountBase, "fresh")); !os.IsNotExist(err) {
		t.Fatal("a failed mount left its directory behind")
	}
}

func TestFormatNeedsTheSerial(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	refusal(t, f.format(ctx, "/dev/sda", "data", "sda"), "系统盘")
	refusal(t, f.format(ctx, "/dev/sdb", "bad label!", "WD-123"), "卷标")
	refusal(t, f.format(ctx, "/dev/sdb", "data", "WD-123"), "卸载")
	inv := f.Inventory
	f.Inventory = func(ctx context.Context) ([]storage.Disk, error) {
		disks, _ := inv(ctx)
		disks[1].Partitions = disks[1].Partitions[:2]
		return disks, nil
	}
	refusal(t, f.format(ctx, "/dev/sdb", "data", "sdb"), "确认文字")
	if len(f.calls) != 0 {
		t.Fatalf("commands ran: %v", f.calls)
	}
}

func TestSharesStayInsideStorage(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	data := f.Bases()[0]
	outside := t.TempDir()
	refusal(t, f.setShares(ctx, []storage.Share{{Name: "x", Path: outside, Guest: true}}), "只能共享")
	refusal(t, f.setShares(ctx, []storage.Share{{Name: "x", Path: "/etc", Guest: true}}), "系统目录")
	refusal(t, f.setShares(ctx, []storage.Share{{Name: "x", Path: data + "/../data", Guest: true}}), "完整路径")
	link := filepath.Join(data, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	refusal(t, f.setShares(ctx, []storage.Share{{Name: "x", Path: link, Guest: true}}), "只能共享")
	refusal(t, f.setShares(ctx, []storage.Share{{Name: "global", Path: data, Guest: true}}), "名称")
	refusal(t, f.setShares(ctx, []storage.Share{{Name: "a]\n[evil", Path: data, Guest: true}}), "名称")
	refusal(t, f.setShares(ctx, []storage.Share{{Name: "a", Path: data, Guest: true}, {Name: "A", Path: data, Guest: true}}), "重复")
	refusal(t, f.setShares(ctx, []storage.Share{{Name: "照片", Path: data}}), "共享账号")
	if len(f.calls) != 0 {
		t.Fatalf("commands ran: %v", f.calls)
	}
}

func TestSharesAreWrittenAndValidated(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	data := f.Bases()[0]
	if err := f.sharePassword(ctx, "short"); err == nil {
		t.Fatal("short password accepted")
	}
	if err := f.sharePassword(ctx, "correct horse"); err != nil {
		t.Fatal(err)
	}
	if c := f.calls[len(f.calls)-1]; c.argv[0] != "smbpasswd" || c.stdin != "correct horse\ncorrect horse\n" {
		t.Fatalf("smbpasswd call = %+v", c)
	}
	shares := []storage.Share{{Name: "照片", Path: data, Writable: true}, {Name: "public", Path: data, Guest: true}}
	if err := f.setShares(ctx, shares); err != nil {
		t.Fatal(err)
	}
	inc, _ := os.ReadFile(f.SambaInclude)
	for _, want := range []string{"[照片]", "path = " + data, "read only = no", "valid users = " + f.ShareUser, "[public]", "guest ok = yes", "read only = yes", "force user = " + f.ServiceUser} {
		if !strings.Contains(string(inc), want) {
			t.Fatalf("include lacks %q:\n%s", want, inc)
		}
	}
	conf, _ := os.ReadFile(f.SambaConf)
	if strings.Count(string(conf), "include = "+f.SambaInclude) != 1 {
		t.Fatalf("smb.conf:\n%s", conf)
	}
	if err := f.setShares(ctx, shares[:1]); err != nil {
		t.Fatal(err)
	}
	if conf2, _ := os.ReadFile(f.SambaConf); string(conf2) != string(conf) {
		t.Fatal("include directive added twice")
	}
	// A configuration testparm rejects is rolled back.
	before, _ := os.ReadFile(f.SambaInclude)
	f.fail["testparm"] = true
	if err := f.setShares(ctx, shares); err == nil {
		t.Fatal("rejected configuration kept")
	}
	if after, _ := os.ReadFile(f.SambaInclude); string(after) != string(before) {
		t.Fatal("include not restored")
	}
	st, _ := f.status()
	if len(st.Shares) != 1 || !st.ShareUser {
		t.Fatalf("status = %+v", st)
	}
}

func TestNetworkMountKeepsPasswordOutOfState(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	refusal(t, f.addNet(ctx, &storage.NetMount{Name: "nas", Kind: "smb", Server: "evil;rm", Share: "x"}), "服务器")
	refusal(t, f.addNet(ctx, &storage.NetMount{Name: "nas", Kind: "smb", Server: "10.0.0.2", Share: "a,b"}), "共享名称")
	refusal(t, f.addNet(ctx, &storage.NetMount{Name: "nas", Kind: "smb", Server: "10.0.0.2", Share: "../x"}), "共享名称")
	refusal(t, f.addNet(ctx, &storage.NetMount{Name: "nas", Kind: "ftp", Server: "10.0.0.2", Share: "x"}), "SMB")
	refusal(t, f.addNet(ctx, &storage.NetMount{Name: "nas", Kind: "smb", Server: "10.0.0.2", Share: "x", Username: "a,b"}), "用户名")
	if err := f.addNet(ctx, &storage.NetMount{Name: "nas", Kind: "smb", Server: "nas.local", Share: "/media/", Username: "me", Password: "s3cret", Persist: true}); err != nil {
		t.Fatal(err)
	}
	cred := filepath.Join(f.CredDir, "nas")
	info, err := os.Stat(cred)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("credentials: %v %v", info, err)
	}
	if b, _ := os.ReadFile(cred); string(b) != "username=me\npassword=s3cret\n" {
		t.Fatalf("credentials = %q", b)
	}
	last := f.calls[len(f.calls)-1].argv
	if last[0] != "mount" || last[2] != "cifs" || last[5] != "//nas.local/media" || strings.Contains(last[4], "s3cret") {
		t.Fatalf("mount = %v", last)
	}
	if b, _ := os.ReadFile(f.StateFile); bytes.Contains(b, []byte("s3cret")) {
		t.Fatal("password stored in state")
	}
	if b, _ := os.ReadFile(f.Fstab); !strings.Contains(string(b), "//nas.local/media "+filepath.Join(f.MountBase, "nas")+" cifs credentials=") {
		t.Fatalf("fstab:\n%s", b)
	}
	refusal(t, f.addNet(ctx, &storage.NetMount{Name: "nas", Kind: "nfs", Server: "10.0.0.2", Share: "x"}), "同名")
	if err := f.removeNet(ctx, "nas"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cred); !os.IsNotExist(err) {
		t.Fatal("credentials left behind")
	}
	if b, _ := os.ReadFile(f.Fstab); strings.Contains(string(b), "nas") {
		t.Fatalf("fstab:\n%s", b)
	}
}

func TestServeAnswersOneRequest(t *testing.T) {
	f := newFixture(t)
	var out bytes.Buffer
	f.Serve(context.Background(), strings.NewReader(`{"op":"status","extra":1}`), &out)
	var res storage.Response
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || res.OK {
		t.Fatalf("unknown field accepted: %s", out.String())
	}
	out.Reset()
	f.Serve(context.Background(), strings.NewReader(`{"op":"mount","device":"/dev/sda2","name":"x"}`), &out)
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || res.OK || !strings.Contains(res.Error, "系统盘") {
		t.Fatalf("response = %s", out.String())
	}
	out.Reset()
	f.Serve(context.Background(), strings.NewReader(`{"op":"status"}`), &out)
	var st storage.Status
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || !res.OK || json.Unmarshal(res.Data, &st) != nil || !st.Tools["smbd"] {
		t.Fatalf("response = %s", out.String())
	}
}

func TestAutomountOnlyWhenEnabled(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.automount(ctx, "/dev/sdb1"); err != nil || len(f.calls) != 0 {
		t.Fatalf("automount while disabled: %v %v", err, f.calls)
	}
	if err := f.update(func(s *state) error { s.Automount = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := f.automount(ctx, "/dev/sda2"); err != nil || len(f.calls) != 0 {
		t.Fatalf("system disk automounted: %v %v", err, f.calls)
	}
	if err := f.automount(ctx, "/dev/sdb1"); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || f.calls[0].argv[0] != "systemd-mount" || !strings.Contains(strings.Join(f.calls[0].argv, " "), "--collect") {
		t.Fatalf("calls = %v", f.calls)
	}
}

func TestParseSmart(t *testing.T) {
	ata := `{"model_name":"WDC WD40","smart_status":{"passed":true},"temperature":{"current":38},"power_on_time":{"hours":12345},
"ata_smart_attributes":{"table":[{"id":5,"raw":{"value":0}},{"id":197,"raw":{"value":3}}]}}`
	s := ParseSmart([]byte(ata))
	if !s.Available || !*s.Passed || *s.TemperatureC != 38 || *s.PowerOnHours != 12345 || *s.Reallocated != 0 || *s.Pending != 3 || s.Model != "WDC WD40" {
		t.Fatalf("ata = %+v", s)
	}
	nvme := `{"smart_status":{"passed":false},"nvme_smart_health_information_log":{"percentage_used":7}}`
	s = ParseSmart([]byte(nvme))
	if *s.Passed || *s.PercentUsed != 7 {
		t.Fatalf("nvme = %+v", s)
	}
	usb := `{"smartctl":{"messages":[{"string":"/dev/sdb: Unknown USB bridge [0x1234:0x5678]","severity":"error"}]}}`
	s = ParseSmart([]byte(usb))
	if s.Available || !strings.Contains(s.Detail, "USB bridge") {
		t.Fatalf("usb = %+v", s)
	}
}

func TestClientOverSocket(t *testing.T) {
	f := newFixture(t)
	sock := filepath.Join(f.dir, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip("unix sockets unavailable:", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			f.Serve(context.Background(), conn, conn)
			conn.Close()
		}
	}()
	c := storage.Client{Socket: sock}
	var disks []storage.Disk
	if err := c.Call(context.Background(), storage.Request{Op: "disks"}, &disks); err != nil || len(disks) != 2 {
		t.Fatal(disks, err)
	}
	err = c.Call(context.Background(), storage.Request{Op: "format", Device: "/dev/sda", Label: "x", Confirm: "sda"}, nil)
	var he *storage.HelperError
	if !errors.As(err, &he) || !strings.Contains(he.Message, "系统盘") {
		t.Fatal(err)
	}
	if err := (storage.Client{Socket: filepath.Join(f.dir, "none")}).Call(context.Background(), storage.Request{Op: "status"}, nil); !errors.Is(err, storage.ErrHelperMissing) {
		t.Fatal(err)
	}
}
