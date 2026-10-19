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
	"time"

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
	// Seagate packs counters into 48-bit raw values; on 32-bit ARM these
	// must not make the whole report unreadable.
	seagate := `{"smart_status":{"passed":true},"temperature":{"current":34},"power_on_time":{"hours":1757},
"ata_smart_attributes":{"table":[{"id":5,"raw":{"value":0}},{"id":9,"raw":{"value":15195787566778077}},{"id":194,"raw":{"value":90194313249}},{"id":197,"raw":{"value":0}}]}}`
	s = ParseSmart([]byte(seagate))
	if !s.Available || *s.TemperatureC != 34 || *s.PowerOnHours != 1757 || *s.Reallocated != 0 {
		t.Fatalf("seagate = %+v", s)
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

func TestAutomountLeavesHostFstabDisksAlone(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.update(func(s *state) error { s.Automount = true; return nil }); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"UUID=1111-2222  /srv/data  ext4  defaults,nofail  0  2",
		"LABEL=Backup\\040Disk /srv/backup ext4 defaults 0 2",
		"/dev/sdb1 /srv/data ext4 defaults 0 2",
	} {
		if err := os.WriteFile(f.Fstab, []byte("# UUID=nothing\n"+line+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		f.calls = nil
		if err := f.automount(ctx, "/dev/sdb1"); err != nil || len(f.calls) != 0 {
			t.Fatalf("%q: automounted a host fstab disk: %v %v", line, err, f.calls)
		}
	}
	// A line inside the nasctl block is nasctl's own and does not count.
	if err := os.WriteFile(f.Fstab, []byte(fstabBegin+"\nUUID=1111-2222 /media/nasctl/x ext4 nofail 0 2\n"+fstabEnd+"\n# UUID=1111-2222 /old ext4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.automount(ctx, "/dev/sdb1"); err != nil || len(f.calls) != 1 {
		t.Fatalf("calls = %v, err = %v", f.calls, err)
	}
}

func TestSmartRetriesUnknownUSBBridgeThroughSAT(t *testing.T) {
	f := newFixture(t)
	f.Run = func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
		f.calls = append(f.calls, call{argv: append([]string{name}, args...)})
		if strings.Contains(strings.Join(args, " "), "-d sat") {
			return []byte(`{"smart_status":{"passed":true},"temperature":{"current":33}}`), nil
		}
		return []byte(`{"smartctl":{"messages":[{"string":"/dev/sdb: Unknown USB bridge [0x0bda:0x9201 (0xf200)]"}]}}`), errors.New("exit status 1")
	}
	s, err := f.smart(context.Background(), "/dev/sdb")
	if err != nil || !s.Available || !*s.Passed || *s.TemperatureC != 33 || len(f.calls) != 2 {
		t.Fatalf("smart = %+v, %v, calls %v", s, err, f.calls)
	}
}

func TestPersistReplacesAnEarlierNameForThePartition(t *testing.T) {
	f := newFixture(t)
	original := "UUID=abc / ext4 defaults 0 1\n"
	if err := os.WriteFile(f.Fstab, []byte(original), 0o664); err != nil {
		t.Fatal(err)
	}
	// Set the mode explicitly: WriteFile's is masked by the umask.
	if err := os.Chmod(f.Fstab, 0o664); err != nil {
		t.Fatal(err)
	}
	if err := f.mount(context.Background(), "/dev/sdb1", "first", true); err != nil {
		t.Fatal(err)
	}
	// Unmounted without forgetting, then mounted at boot under a new name.
	if err := f.mount(context.Background(), "/dev/sdb1", "second", true); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f.Fstab)
	if n := strings.Count(string(b), "UUID=1111-2222 "); n != 1 || !strings.Contains(string(b), filepath.Join(f.MountBase, "second")) {
		t.Fatalf("fstab:\n%s", b)
	}
	if info, _ := os.Stat(f.Fstab); info.Mode().Perm() != 0o664 {
		t.Fatalf("fstab mode = %v, want the original 0664", info.Mode().Perm())
	}
}

func TestNTFSPrefersTheKernelDriverAndKeepsFUSEAlive(t *testing.T) {
	f := newFixture(t)
	f.Inventory = func(context.Context) ([]storage.Disk, error) {
		return []storage.Disk{{Name: "sdc", Path: "/dev/sdc", Transport: "usb", Partitions: []storage.Partition{
			{Name: "sdc1", Path: "/dev/sdc1", FSType: "ntfs", Label: "Win", UUID: "AAAA"}}}}, nil
	}
	f.Filesystems = filepath.Join(f.dir, "filesystems")
	if err := os.WriteFile(f.Filesystems, []byte("nodev\tproc\n\text4\n\tntfs3\n\tfuseblk\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := f.mount(ctx, "/dev/sdc1", "", false); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.calls[0].argv[:3], " "); got != "mount -t ntfs3" {
		t.Fatalf("with ntfs3 in the kernel: %v", f.calls)
	}
	// Only ntfs-3g: a FUSE daemon, which must not live in the helper's unit.
	if err := os.WriteFile(f.Filesystems, []byte("\text4\n\tfuseblk\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	if err := f.mount(ctx, "/dev/sdc1", "win2", false); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.calls[0].argv[:4], " "); got != "systemd-mount --collect -t ntfs-3g" {
		t.Fatalf("with only ntfs-3g: %v", f.calls)
	}
}

func TestShareNamesDoNotTakeOverHostShares(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(f.SambaConf, []byte("[global]\n   workgroup = WORKGROUP\n\n  [Data]\n   path = /srv/data\n   valid users = ty0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := f.setShares(context.Background(), []storage.Share{{Name: "data", Path: f.Bases()[0], Guest: true}})
	refusal(t, err, "已有同名共享")
	if _, statErr := os.Stat(f.SambaInclude); !os.IsNotExist(statErr) {
		t.Fatalf("include written despite the refusal: %v", statErr)
	}
}

func TestRunStopsChildrenThatHoldTheOutput(t *testing.T) {
	// Like mount handing over to mount.nfs: the child keeps stdout open.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := run(ctx, nil, "sh", "-c", "sleep 30 & sleep 30"); err == nil {
		t.Fatal("no error after the deadline")
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Fatalf("run returned after %v", d)
	}
}

func TestNFSMountsOnceButBootRetries(t *testing.T) {
	f := newFixture(t)
	if err := f.addNet(context.Background(), &storage.NetMount{Name: "nas", Kind: "nfs", Server: "10.0.0.2", Share: "export", Persist: true}); err != nil {
		t.Fatal(err)
	}
	if argv := strings.Join(f.calls[0].argv, " "); !strings.Contains(argv, ",retry=0 ") {
		t.Fatalf("mount = %s", argv)
	}
	if b, _ := os.ReadFile(f.Fstab); strings.Contains(string(b), "retry=0") || !strings.Contains(string(b), "10.0.0.2:/export") {
		t.Fatalf("fstab:\n%s", b)
	}
}
