// Package storagehelper is the root side of storage management. systemd
// starts it per connection on the helper socket (nasctl-storage.socket); it
// reads one request, carries it out and exits, so nothing privileged stays
// resident.
//
// The console that connects is unprivileged and may be compromised, so the
// helper trusts nothing it is told: devices are looked up in the live disk
// inventory, the system disk is never touched, mounts go only under
// storage.MountBase, shared folders must lie inside a configured storage
// location or the mount base, and every name is checked against a strict
// pattern before it reaches a command line or a configuration file.
package storagehelper

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/storage"
)

// Helper carries out requests. Paths and the command runner are fields so
// tests can point them at a temporary tree and a recorder.
type Helper struct {
	MountBase    string
	Fstab        string
	SambaConf    string
	SambaInclude string
	StateFile    string
	CredDir      string
	MountInfo    string
	Filesystems  string // /proc/filesystems: the kernel's filesystem drivers
	ServiceUser  string
	ShareUser    string
	// Bases lists the folders shares may be made inside, besides MountBase.
	Bases     func() []string
	Inventory func(context.Context) ([]storage.Disk, error)
	Run       func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error)
	LookPath  func(string) (string, error)
}

// New is the helper for the live system.
func New(bases func() []string) *Helper {
	return &Helper{
		MountBase:    storage.MountBase,
		Fstab:        "/etc/fstab",
		SambaConf:    "/etc/samba/smb.conf",
		SambaInclude: "/etc/samba/nasctl-shares.conf",
		StateFile:    "/etc/nasctl/storage.json",
		CredDir:      "/etc/nasctl/credentials",
		MountInfo:    "/proc/self/mountinfo",
		Filesystems:  "/proc/filesystems",
		ServiceUser:  "nasctl",
		ShareUser:    "nasctl-share",
		Bases:        bases,
		Inventory:    inventory,
		Run:          run,
		LookPath:     exec.LookPath,
	}
}

// inventory lists the disks. Loop, RAM, ROM and SD-card boot areas are left
// out: none of them is somewhere a user keeps files.
func inventory(ctx context.Context) ([]storage.Disk, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "lsblk", "-J", "-b", "-o", storage.LsblkColumns).Output()
	if err != nil {
		out, err = exec.CommandContext(ctx, "lsblk", "-J", "-b", "-o", storage.LsblkColumnsOld).Output()
	}
	if err != nil {
		return nil, errors.New("storage: lsblk is not available")
	}
	disks, err := storage.ParseLsblk(out)
	if err != nil {
		return nil, err
	}
	if swaps, err := os.ReadFile("/proc/swaps"); err == nil {
		storage.MarkSwapFiles(disks, swaps)
	}
	return disks, nil
}

func run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	// mount starts mount.nfs or mount.cifs, which keep the output pipe open;
	// on a deadline the whole group is killed and Wait stops waiting for the
	// pipe, so an unreachable server cannot hold a request past it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	b := out.Bytes()
	if len(b) > 64<<10 {
		b = b[len(b)-64<<10:]
	}
	return b, err
}

// fail is a refusal with a message for people and, optionally, the
// technical detail behind it.
type fail struct{ msg, detail string }

func (f *fail) Error() string { return f.msg }

func refuse(msg string) error { return &fail{msg: msg} }
func failed(msg string, out []byte, err error) error {
	detail := strings.TrimSpace(string(out))
	if detail == "" && err != nil {
		detail = err.Error()
	}
	if len(detail) > 600 {
		detail = detail[len(detail)-600:]
	}
	return &fail{msg: msg, detail: detail}
}

// Serve reads one request from r and writes one response to w.
func (h *Helper) Serve(ctx context.Context, r io.Reader, w io.Writer) {
	var req storage.Request
	res := storage.Response{}
	dec := json.NewDecoder(io.LimitReader(r, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		res.Error = "请求格式无效"
	} else if data, err := h.Handle(ctx, req); err != nil {
		var f *fail
		if errors.As(err, &f) {
			res.Error, res.Detail = f.msg, f.detail
		} else {
			res.Error, res.Detail = "存储操作没有完成", err.Error()
		}
	} else {
		res.OK = true
		if data != nil {
			res.Data, _ = json.Marshal(data)
		}
	}
	_ = json.NewEncoder(w).Encode(res)
}

// Handle dispatches one request.
func (h *Helper) Handle(ctx context.Context, req storage.Request) (any, error) {
	switch req.Op {
	case "status":
		return h.status()
	case "disks":
		// The console cannot see devices (PrivateDevices=yes) and lives in its
		// own mount namespace, so it asks here.
		disks, err := h.Inventory(ctx)
		if err != nil {
			return nil, failed("无法读取磁盘列表", nil, err)
		}
		return disks, nil
	case "smart":
		return h.smart(ctx, req.Device)
	case "mount":
		return nil, h.mount(ctx, req.Device, req.Name, req.Persist)
	case "unmount":
		return nil, h.unmount(ctx, req.Name, req.Forget)
	case "format":
		return nil, h.format(ctx, req.Device, req.Label, req.Confirm)
	case "automount.set":
		return nil, h.update(func(s *state) error { s.Automount = req.Enabled; return nil })
	case "automount":
		return nil, h.automount(ctx, req.Device)
	case "shares.set":
		return nil, h.setShares(ctx, req.Shares)
	case "shares.password":
		return nil, h.sharePassword(ctx, req.Password)
	case "net.add":
		return nil, h.addNet(ctx, req.Net)
	case "net.remove":
		return nil, h.removeNet(ctx, req.Name)
	}
	return nil, refuse("不支持的存储操作")
}

// ---- state ----------------------------------------------------------------

type state struct {
	Automount bool               `json:"automount"`
	Shares    []storage.Share    `json:"shares"`
	NetMounts []storage.NetMount `json:"net_mounts"`
	ShareUser bool               `json:"share_user"`
}

func (h *Helper) load() (state, error) {
	s := state{Shares: []storage.Share{}, NetMounts: []storage.NetMount{}}
	b, err := os.ReadFile(h.StateFile)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil || len(b) > 1<<20 {
		return s, errors.New("storage state unreadable")
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, errors.New("storage state unreadable")
	}
	return s, nil
}

func (h *Helper) update(change func(*state) error) error {
	s, err := h.load()
	if err != nil {
		return err
	}
	if err := change(&s); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	return writeFile(h.StateFile, b, 0o600)
}

func writeFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (h *Helper) status() (storage.Status, error) {
	s, err := h.load()
	if err != nil {
		return storage.Status{}, err
	}
	tools := map[string]bool{}
	for _, t := range []string{"smartctl", "smbd", "smbpasswd", "mount.cifs", "mount.nfs", "mkfs.ext4", "sfdisk", "wipefs", "systemd-mount", "mkfs.exfat", "mount.ntfs-3g"} {
		_, e := h.LookPath(t)
		tools[t] = e == nil
	}
	mounted := h.mounts()
	for i := range s.NetMounts {
		s.NetMounts[i].Password = ""
		s.NetMounts[i].Mounted = mounted[filepath.Join(h.MountBase, s.NetMounts[i].Name)]
	}
	persisted := []string{}
	for _, e := range h.fstabEntries() {
		persisted = append(persisted, e.mountpoint)
	}
	return storage.Status{Tools: tools, Automount: s.Automount, Shares: s.Shares, NetMounts: s.NetMounts, ShareUser: s.ShareUser, Persisted: persisted}, nil
}

// mounts lists the mount points currently in use.
func (h *Helper) mounts() map[string]bool {
	out := map[string]bool{}
	f, err := os.Open(h.MountInfo)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) > 4 {
			out[unescape(fields[4])] = true
		}
	}
	return out
}

func unescape(s string) string {
	r := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return r.Replace(s)
}

// ---- validation -----------------------------------------------------------

var (
	namePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)
	labelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,15}$`)
	sharePattern = regexp.MustCompile(`^[A-Za-z0-9\p{Han}][A-Za-z0-9\p{Han} _.-]{0,31}$`)
	hostPattern  = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,252}[A-Za-z0-9])?$|^\[?[0-9A-Fa-f:]{2,39}\]?$`)
	remotePath   = regexp.MustCompile(`^[^\x00-\x1f"'\\,]{1,255}$`)
	userPattern  = regexp.MustCompile(`^[^\x00-\x1f\s,=]{1,64}$`)
	ntfs3Driver  = regexp.MustCompile(`(?m)\sntfs3$`)
)

// allowedFS maps a filesystem to how it is mounted.
var allowedFS = map[string]string{"ext4": "native", "ext3": "native", "ext2": "native", "xfs": "native", "btrfs": "native", "f2fs": "native", "vfat": "foreign", "exfat": "foreign", "ntfs": "foreign"}

func (h *Helper) findPartition(ctx context.Context, device string) (storage.Disk, storage.Partition, error) {
	disks, err := h.Inventory(ctx)
	if err != nil {
		return storage.Disk{}, storage.Partition{}, refuse("无法读取磁盘列表")
	}
	for _, d := range disks {
		for _, p := range d.Partitions {
			if p.Path == device {
				return d, p, nil
			}
		}
	}
	return storage.Disk{}, storage.Partition{}, refuse("找不到这个分区，磁盘可能已被拔出")
}

func (h *Helper) mountpoint(name string) (string, error) {
	if !namePattern.MatchString(name) {
		return "", refuse("名称只能包含字母、数字、点、下划线和短横线，最多 32 个字符")
	}
	return filepath.Join(h.MountBase, name), nil
}

// ---- mounting -------------------------------------------------------------

func (h *Helper) mountOptions(fs string) (string, error) {
	switch allowedFS[fs] {
	case "native":
		return "noatime,nosuid,nodev", nil
	case "foreign":
		u, err := user.Lookup(h.ServiceUser)
		if err != nil {
			return "", refuse("找不到 nasctl 服务用户")
		}
		return fmt.Sprintf("uid=%s,gid=%s,umask=0002,noatime,nosuid,nodev", u.Uid, u.Gid), nil
	}
	return "", refuse("不支持挂载 " + fs + " 文件系统")
}

// fsType is the type handed to mount: NTFS uses the in-kernel ntfs3 driver,
// or ntfs-3g when only that is installed.
func (h *Helper) fsType(fs string) string {
	if fs == "ntfs" {
		if b, err := os.ReadFile(h.Filesystems); err == nil && ntfs3Driver.Match(b) {
			return "ntfs3"
		}
		if _, err := h.LookPath("mount.ntfs-3g"); err == nil {
			return "ntfs-3g"
		}
		return "ntfs3"
	}
	return fs
}

// mountCommand mounts dev at mp. A FUSE filesystem (ntfs-3g) keeps a daemon
// running for as long as it is mounted; started from the helper, systemd
// would stop that daemon with the helper's own unit when the request ends.
// systemd-mount gives it a mount unit of its own.
func (h *Helper) mountCommand(ctx context.Context, fstype, opts, dev, mp string) ([]byte, error) {
	if fstype == "ntfs-3g" {
		if _, err := h.LookPath("systemd-mount"); err == nil {
			return h.Run(ctx, nil, "systemd-mount", "--collect", "-t", fstype, "-o", opts, dev, mp)
		}
	}
	return h.Run(ctx, nil, "mount", "-t", fstype, "-o", opts, dev, mp)
}

func (h *Helper) mount(ctx context.Context, device, name string, persist bool) error {
	disk, part, err := h.findPartition(ctx, device)
	if err != nil {
		return err
	}
	if disk.System || part.System {
		return refuse("这是系统盘，不能在这里挂载或修改")
	}
	if len(part.Mountpoints) > 0 {
		return refuse("这个分区已经挂载在 " + part.Mountpoints[0])
	}
	if name == "" {
		name = defaultName(part)
	}
	mp, err := h.mountpoint(name)
	if err != nil {
		return err
	}
	opts, err := h.mountOptions(part.FSType)
	if err != nil {
		return err
	}
	if err := h.prepareMountpoint(mp); err != nil {
		return err
	}
	if out, err := h.mountCommand(ctx, h.fsType(part.FSType), opts, part.Path, mp); err != nil {
		_ = os.Remove(mp)
		return failed("挂载失败", out, err)
	}
	if persist {
		if part.UUID == "" {
			return refuse("已挂载，但这个分区没有 UUID，不能设置开机自动挂载")
		}
		pass := "0"
		if strings.HasPrefix(part.FSType, "ext") {
			pass = "2"
		}
		// The partition may still be remembered under a name it was mounted
		// with before; two lines would mount it twice at boot.
		for _, e := range h.fstabEntries() {
			if f := strings.Fields(e.line); e.mountpoint != mp && f[0] == "UUID="+part.UUID {
				if err := h.setFstab(e.mountpoint, ""); err != nil {
					return err
				}
			}
		}
		return h.setFstab(mp, fmt.Sprintf("UUID=%s %s %s %s,nofail,x-systemd.device-timeout=10s 0 %s", part.UUID, escapeFstab(mp), h.fsType(part.FSType), opts, pass))
	}
	return nil
}

// prepareMountpoint creates an empty directory under the mount base, or
// refuses a name already in use.
func (h *Helper) prepareMountpoint(mp string) error {
	if err := os.MkdirAll(h.MountBase, 0o755); err != nil {
		return err
	}
	if h.mounts()[mp] {
		return refuse("这个名称已被另一个挂载使用")
	}
	if entries, err := os.ReadDir(mp); err == nil && len(entries) > 0 {
		return refuse("同名目录里已有文件，请换一个名称")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if info, err := os.Lstat(mp); err == nil && !info.IsDir() {
		return refuse("同名位置不是目录，请换一个名称")
	}
	return os.MkdirAll(mp, 0o755)
}

func defaultName(p storage.Partition) string {
	var b strings.Builder
	for _, r := range p.Label {
		if r < 128 && (r == '.' || r == '_' || r == '-' || r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z') {
			b.WriteRune(r)
		}
	}
	name := strings.TrimLeft(b.String(), "._-")
	if name == "" {
		uuid := strings.ReplaceAll(p.UUID, "-", "")
		if len(uuid) > 8 {
			uuid = uuid[:8]
		}
		name = "disk-" + strings.ToLower(uuid)
		if uuid == "" {
			name = "disk-" + p.Name
		}
	}
	if len(name) > 32 {
		name = name[:32]
	}
	return name
}

func (h *Helper) unmount(ctx context.Context, name string, forget bool) error {
	mp, err := h.mountpoint(name)
	if err != nil {
		return err
	}
	if !h.mounts()[mp] {
		if forget {
			return h.setFstab(mp, "")
		}
		return refuse("没有挂载在这个名称下的磁盘")
	}
	if out, err := h.Run(ctx, nil, "umount", mp); err != nil {
		return failed("卸载失败：可能还有程序在使用这个磁盘上的文件", out, err)
	}
	_ = os.Remove(mp)
	if forget {
		return h.setFstab(mp, "")
	}
	return nil
}

func (h *Helper) automount(ctx context.Context, device string) error {
	s, err := h.load()
	if err != nil || !s.Automount {
		return err
	}
	disk, part, err := h.findPartition(ctx, device)
	if err != nil {
		return err
	}
	if disk.System || part.System || len(part.Mountpoints) > 0 || (!disk.Removable && disk.Transport != "usb") || allowedFS[part.FSType] == "" {
		return nil
	}
	// A USB data disk the machine mounts from its own fstab lines may appear
	// before that mount happens (nofail mounts don't hold up boot), or again
	// after a reconnect; it belongs where the fstab puts it.
	if h.inHostFstab(part) {
		return nil
	}
	base := defaultName(part)
	name := base
	for i := 2; h.mounts()[filepath.Join(h.MountBase, name)] && i < 10; i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	mp, err := h.mountpoint(name)
	if err != nil {
		return err
	}
	opts, err := h.mountOptions(part.FSType)
	if err != nil {
		return err
	}
	if err := h.prepareMountpoint(mp); err != nil {
		return err
	}
	// systemd-mount ties the mount to the device, so unplugging it unmounts
	// the filesystem instead of leaving a dead mount behind.
	if _, err := h.LookPath("systemd-mount"); err == nil {
		out, err := h.Run(ctx, nil, "systemd-mount", "--no-block", "--collect", "-t", h.fsType(part.FSType), "-o", opts, part.Path, mp)
		if err != nil {
			return failed("自动挂载失败", out, err)
		}
		return nil
	}
	out, err := h.mountCommand(ctx, h.fsType(part.FSType), opts, part.Path, mp)
	if err != nil {
		return failed("自动挂载失败", out, err)
	}
	return nil
}

// ---- formatting -----------------------------------------------------------

func (h *Helper) format(ctx context.Context, device, label, confirm string) error {
	if !labelPattern.MatchString(label) {
		return refuse("卷标只能包含字母、数字、下划线和短横线，最多 16 个字符")
	}
	disks, err := h.Inventory(ctx)
	if err != nil {
		return refuse("无法读取磁盘列表")
	}
	var disk *storage.Disk
	for i := range disks {
		if disks[i].Path == device {
			disk = &disks[i]
		}
	}
	if disk == nil {
		return refuse("找不到这块磁盘，可能已被拔出")
	}
	if disk.System {
		return refuse("这是系统盘，不能格式化")
	}
	if disk.ReadOnly {
		return refuse("这块磁盘是只读的")
	}
	for _, p := range disk.Partitions {
		if len(p.Mountpoints) > 0 {
			return refuse("请先卸载这块磁盘上的所有分区")
		}
	}
	want := disk.Serial
	if want == "" {
		want = disk.Name
	}
	if confirm != want {
		return refuse("确认文字不对：请输入这块磁盘的序列号（没有序列号时输入设备名）")
	}
	for _, step := range [][]string{{"wipefs", "-a", "-f", disk.Path}} {
		if out, err := h.Run(ctx, nil, step[0], step[1:]...); err != nil {
			return failed("清除旧分区表失败", out, err)
		}
	}
	if out, err := h.Run(ctx, []byte("label: gpt\n,,L\n"), "sfdisk", "--quiet", "--wipe", "always", disk.Path); err != nil {
		return failed("创建分区失败", out, err)
	}
	_, _ = h.Run(ctx, nil, "udevadm", "settle", "--timeout=10")
	part := disk.Path + "1"
	if last := disk.Path[len(disk.Path)-1]; last >= '0' && last <= '9' {
		part = disk.Path + "p1"
	}
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(part); err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if out, err := h.Run(ctx, nil, "mkfs.ext4", "-F", "-q", "-L", label, part); err != nil {
		return failed("格式化失败", out, err)
	}
	_, _ = h.Run(ctx, nil, "udevadm", "settle", "--timeout=10")
	if err := h.mount(ctx, part, label, true); err != nil {
		return err
	}
	// A fresh filesystem belongs to root; hand its top folder to the service
	// user so the file browser can write to it.
	if u, err := user.Lookup(h.ServiceUser); err == nil {
		uid, _ := strconv.Atoi(u.Uid)
		gid, _ := strconv.Atoi(u.Gid)
		_ = os.Chown(filepath.Join(h.MountBase, label), uid, gid)
	}
	return nil
}

// ---- SMART ----------------------------------------------------------------

func (h *Helper) smart(ctx context.Context, device string) (storage.SMART, error) {
	disks, err := h.Inventory(ctx)
	if err != nil {
		return storage.SMART{}, refuse("无法读取磁盘列表")
	}
	found := false
	for _, d := range disks {
		found = found || d.Path == device
	}
	if !found {
		return storage.SMART{}, refuse("找不到这块磁盘")
	}
	if _, err := h.LookPath("smartctl"); err != nil {
		return storage.SMART{Detail: "未安装 smartmontools"}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, _ := h.Run(ctx, nil, "smartctl", "-j", "-H", "-A", "-i", device)
	s := ParseSmart(out)
	// Many USB enclosures (Realtek, some JMicron) pass ATA commands through
	// SAT but are missing from smartctl's bridge list.
	if !s.Available && strings.Contains(s.Detail, "Unknown USB bridge") {
		out, _ = h.Run(ctx, nil, "smartctl", "-j", "-d", "sat", "-H", "-A", "-i", device)
		if sat := ParseSmart(out); sat.Available {
			return sat, nil
		}
	}
	return s, nil
}

// ParseSmart reads the parts of `smartctl -j` worth showing.
func ParseSmart(out []byte) storage.SMART {
	var doc struct {
		Model       string `json:"model_name"`
		SmartStatus *struct {
			Passed bool `json:"passed"`
		} `json:"smart_status"`
		Temperature *struct {
			Current int `json:"current"`
		} `json:"temperature"`
		PowerOn *struct {
			Hours int `json:"hours"`
		} `json:"power_on_time"`
		ATA *struct {
			Table []struct {
				ID  int `json:"id"`
				Raw struct {
					// Vendors pack several counters into one raw value
					// (Seagate's power-on hours and temperature), past 32 bits.
					Value int64 `json:"value"`
				} `json:"raw"`
			} `json:"table"`
		} `json:"ata_smart_attributes"`
		NVMe *struct {
			PercentUsed *int `json:"percentage_used"`
		} `json:"nvme_smart_health_information_log"`
		Smartctl struct {
			Messages []struct {
				String string `json:"string"`
			} `json:"messages"`
		} `json:"smartctl"`
	}
	s := storage.SMART{}
	if json.Unmarshal(out, &doc) != nil {
		s.Detail = "smartctl 没有返回可读的结果"
		return s
	}
	s.Model = doc.Model
	if doc.SmartStatus != nil {
		passed := doc.SmartStatus.Passed
		s.Passed, s.Available = &passed, true
	}
	if doc.Temperature != nil && doc.Temperature.Current > 0 {
		t := doc.Temperature.Current
		s.TemperatureC = &t
	}
	if doc.PowerOn != nil {
		hours := doc.PowerOn.Hours
		s.PowerOnHours = &hours
	}
	if doc.ATA != nil {
		for _, a := range doc.ATA.Table {
			v := int(a.Raw.Value)
			switch a.ID {
			case 5:
				s.Reallocated = &v
			case 197:
				s.Pending = &v
			}
		}
	}
	if doc.NVMe != nil {
		s.PercentUsed = doc.NVMe.PercentUsed
	}
	if !s.Available && len(doc.Smartctl.Messages) > 0 {
		s.Detail = doc.Smartctl.Messages[0].String
	}
	return s
}

// ---- fstab ------------------------------------------------------------------

const (
	fstabBegin = "# BEGIN nasctl managed mounts - edited by nasctl, keep these markers"
	fstabEnd   = "# END nasctl managed mounts"
)

type fstabEntry struct{ mountpoint, line string }

func escapeFstab(s string) string { return strings.ReplaceAll(s, " ", `\040`) }

func (h *Helper) fstabEntries() []fstabEntry {
	b, err := os.ReadFile(h.Fstab)
	if err != nil {
		return nil
	}
	out := []fstabEntry{}
	inside := false
	for _, line := range strings.Split(string(b), "\n") {
		switch {
		case line == fstabBegin:
			inside = true
		case line == fstabEnd:
			inside = false
		case inside && strings.TrimSpace(line) != "":
			if f := strings.Fields(line); len(f) >= 2 {
				out = append(out, fstabEntry{unescape(f[1]), line})
			}
		}
	}
	return out
}

// setFstab replaces the managed entry for mp; an empty line removes it. Lines
// outside the nasctl block are kept byte for byte.
func (h *Helper) setFstab(mp, line string) error {
	b, err := os.ReadFile(h.Fstab)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var before, after []string
	entries := map[string]string{}
	inside, seen := false, false
	for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		switch {
		case l == fstabBegin:
			inside, seen = true, true
		case l == fstabEnd:
			inside = false
		case inside:
			if f := strings.Fields(l); len(f) >= 2 {
				entries[unescape(f[1])] = l
			}
		case seen:
			after = append(after, l)
		default:
			before = append(before, l)
		}
	}
	if line == "" {
		delete(entries, mp)
	} else {
		entries[mp] = line
	}
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	out = append(out, before...)
	if len(keys) > 0 {
		out = append(out, fstabBegin)
		for _, k := range keys {
			out = append(out, entries[k])
		}
		out = append(out, fstabEnd)
	}
	out = append(out, after...)
	text := strings.Join(out, "\n")
	if text != "" {
		text += "\n"
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(h.Fstab); err == nil {
		mode = info.Mode().Perm()
	}
	if backup := h.Fstab + ".nasctl-backup"; !exists(backup) && len(b) > 0 {
		_ = writeFile(backup, b, mode)
	}
	return writeFile(h.Fstab, []byte(text), mode)
}

// inHostFstab reports whether a line outside the nasctl block names the
// partition, by UUID, PARTUUID, LABEL or device path.
func (h *Helper) inHostFstab(p storage.Partition) bool {
	b, err := os.ReadFile(h.Fstab)
	if err != nil {
		return false
	}
	names := map[string]bool{p.Path: true}
	if p.UUID != "" {
		names["UUID="+p.UUID] = true
		names["/dev/disk/by-uuid/"+p.UUID] = true
	}
	if p.PartUUID != "" {
		names["PARTUUID="+p.PartUUID] = true
		names["/dev/disk/by-partuuid/"+p.PartUUID] = true
	}
	if p.Label != "" {
		names["LABEL="+escapeFstab(p.Label)] = true
		names["/dev/disk/by-label/"+escapeFstab(p.Label)] = true
	}
	inside := false
	for _, line := range strings.Split(string(b), "\n") {
		switch {
		case line == fstabBegin:
			inside = true
		case line == fstabEnd:
			inside = false
		case !inside:
			f := strings.Fields(line)
			if len(f) > 0 && !strings.HasPrefix(f[0], "#") && names[strings.Trim(f[0], `"`)] {
				return true
			}
		}
	}
	return false
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// ---- Samba ------------------------------------------------------------------

func (h *Helper) allowedShare(p string) error {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return refuse("共享目录必须是完整路径")
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return refuse("共享目录不存在：" + p)
	}
	if info, err := os.Stat(real); err != nil || !info.IsDir() {
		return refuse("共享目录不是文件夹：" + p)
	}
	// Configured storage locations come from a file the console can edit, so
	// system folders are refused even when one is listed there.
	for _, deny := range []string{"/etc", "/usr", "/boot", "/proc", "/sys", "/dev", "/run", "/root", "/var/lib", "/var/log", "/bin", "/sbin", "/lib"} {
		if real == deny || strings.HasPrefix(real, deny+"/") {
			return refuse("不能共享系统目录：" + p)
		}
	}
	if real == "/" {
		return refuse("不能共享整个系统")
	}
	bases := []string{h.MountBase}
	if h.Bases != nil {
		bases = append(bases, h.Bases()...)
	}
	for _, b := range bases {
		rb, err := filepath.EvalSymlinks(b)
		if err != nil {
			continue
		}
		if real == rb || strings.HasPrefix(real, rb+string(filepath.Separator)) {
			return nil
		}
	}
	return refuse("只能共享已配置的存储位置或 " + h.MountBase + " 里的文件夹：" + p)
}

func (h *Helper) setShares(ctx context.Context, shares []storage.Share) error {
	if len(shares) > 32 {
		return refuse("最多 32 个共享")
	}
	seen := map[string]bool{}
	// Samba merges sections that share a name, so a nasctl share named like
	// one in the host's own smb.conf would rewrite that share.
	host := map[string]bool{}
	if b, err := os.ReadFile(h.SambaConf); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if l := strings.TrimSpace(line); strings.HasPrefix(l, "[") && strings.HasSuffix(l, "]") {
				host[strings.ToLower(strings.TrimSpace(l[1:len(l)-1]))] = true
			}
		}
	}
	for _, s := range shares {
		key := strings.ToLower(s.Name)
		if host[key] {
			return refuse("NAS 的 Samba 配置里已有同名共享，请换一个名称：" + s.Name)
		}
		if !sharePattern.MatchString(s.Name) || seen[key] || map[string]bool{"global": true, "homes": true, "printers": true, "print$": true, "ipc$": true}[key] {
			return refuse("共享名称无效或重复：" + s.Name)
		}
		seen[key] = true
		if err := h.allowedShare(s.Path); err != nil {
			return err
		}
	}
	if _, err := h.LookPath("smbd"); err != nil {
		return refuse("没有安装 Samba：请先在 NAS 上执行 apt install samba")
	}
	st, err := h.load()
	if err != nil {
		return err
	}
	for _, s := range shares {
		if !s.Guest && !st.ShareUser {
			return refuse("需要密码的共享要先设置共享账号的密码")
		}
	}
	var b strings.Builder
	b.WriteString("# Managed by nasctl: change shares in the web console; edits here are overwritten.\n")
	for _, s := range shares {
		fmt.Fprintf(&b, "\n[%s]\n   path = %s\n   browseable = yes\n   read only = %s\n", s.Name, s.Path, map[bool]string{true: "no", false: "yes"}[s.Writable])
		if s.Guest {
			b.WriteString("   guest ok = yes\n")
		} else {
			fmt.Fprintf(&b, "   guest ok = no\n   valid users = %s\n", h.ShareUser)
		}
		fmt.Fprintf(&b, "   force user = %s\n   force group = %s\n   create mask = 0664\n   directory mask = 0775\n", h.ServiceUser, h.ServiceUser)
	}
	previous, _ := os.ReadFile(h.SambaInclude)
	if err := writeFile(h.SambaInclude, []byte(b.String()), 0o644); err != nil {
		return err
	}
	if err := h.ensureInclude(); err != nil {
		return err
	}
	if out, err := h.Run(ctx, nil, "testparm", "-s", h.SambaConf); err != nil {
		_ = writeFile(h.SambaInclude, previous, 0o644)
		return failed("Samba 配置校验没有通过，已恢复原来的共享", out, err)
	}
	if err := h.update(func(s *state) error { s.Shares = shares; return nil }); err != nil {
		return err
	}
	if out, err := h.Run(ctx, nil, "smbcontrol", "smbd", "reload-config"); err != nil {
		if out2, err2 := h.Run(ctx, nil, "systemctl", "reload-or-restart", "smbd"); err2 != nil {
			return failed("共享已保存，但 Samba 没有重新加载配置", append(out, out2...), err2)
		}
	}
	return nil
}

func (h *Helper) ensureInclude() error {
	b, err := os.ReadFile(h.SambaConf)
	if err != nil {
		return refuse("找不到 Samba 配置文件 " + h.SambaConf)
	}
	directive := "include = " + h.SambaInclude
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == directive {
			return nil
		}
	}
	text := strings.TrimRight(string(b), "\n") + "\n\n# Shares managed by nasctl\n" + directive + "\n"
	return writeFile(h.SambaConf, []byte(text), 0o644)
}

func (h *Helper) sharePassword(ctx context.Context, password string) error {
	if len(password) < 8 || len(password) > 128 || strings.ContainsAny(password, "\x00\r\n") {
		return refuse("共享密码需要 8 到 128 个字符")
	}
	if _, err := h.LookPath("smbpasswd"); err != nil {
		return refuse("没有安装 Samba：请先在 NAS 上执行 apt install samba")
	}
	if _, err := user.Lookup(h.ShareUser); err != nil {
		if out, err := h.Run(ctx, nil, "useradd", "--system", "--no-create-home", "--shell", "/usr/sbin/nologin", h.ShareUser); err != nil {
			return failed("创建共享账号失败", out, err)
		}
	}
	if out, err := h.Run(ctx, []byte(password+"\n"+password+"\n"), "smbpasswd", "-s", "-a", h.ShareUser); err != nil {
		return failed("设置共享密码失败", out, err)
	}
	return h.update(func(s *state) error { s.ShareUser = true; return nil })
}

// ---- network shares -------------------------------------------------------

func (h *Helper) addNet(ctx context.Context, n *storage.NetMount) error {
	if n == nil {
		return refuse("缺少网络存储信息")
	}
	mp, err := h.mountpoint(n.Name)
	if err != nil {
		return err
	}
	server := strings.Trim(n.Server, "[]")
	if !hostPattern.MatchString(n.Server) || server == "" {
		return refuse("服务器地址无效")
	}
	share := strings.Trim(n.Share, "/")
	if !remotePath.MatchString(share) || strings.Contains(share, "..") {
		return refuse("共享名称或路径无效")
	}
	st, err := h.load()
	if err != nil {
		return err
	}
	for _, m := range st.NetMounts {
		if m.Name == n.Name {
			return refuse("已经有同名的网络存储")
		}
	}
	var fstype, source, opts string
	cred := filepath.Join(h.CredDir, n.Name)
	switch n.Kind {
	case "smb":
		if _, err := h.LookPath("mount.cifs"); err != nil {
			return refuse("没有安装 cifs-utils：请先在 NAS 上执行 apt install cifs-utils")
		}
		if n.Username != "" && !userPattern.MatchString(n.Username) {
			return refuse("用户名无效")
		}
		if strings.ContainsAny(n.Password, "\x00\r\n") || len(n.Password) > 256 {
			return refuse("密码无效")
		}
		u, err := user.Lookup(h.ServiceUser)
		if err != nil {
			return refuse("找不到 nasctl 服务用户")
		}
		creds := "username=guest\npassword=\n"
		if n.Username != "" {
			creds = "username=" + n.Username + "\npassword=" + n.Password + "\n"
		}
		if err := writeFile(cred, []byte(creds), 0o600); err != nil {
			return err
		}
		fstype, source = "cifs", "//"+server+"/"+share
		opts = fmt.Sprintf("credentials=%s,uid=%s,gid=%s,file_mode=0664,dir_mode=0775,iocharset=utf8,nosuid,nodev", cred, u.Uid, u.Gid)
	case "nfs":
		if _, err := h.LookPath("mount.nfs"); err != nil {
			return refuse("没有安装 nfs-common：请先在 NAS 上执行 apt install nfs-common")
		}
		fstype, source, opts = "nfs", server+":/"+share, "soft,timeo=150,retrans=3,nosuid,nodev"
	default:
		return refuse("只支持 SMB 和 NFS")
	}
	if n.ReadOnly {
		opts += ",ro"
	}
	if err := h.prepareMountpoint(mp); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	now := opts
	if fstype == "nfs" {
		now += ",retry=0" // mount.nfs otherwise retries for two minutes
	}
	if out, err := h.Run(ctx, nil, "mount", "-t", fstype, "-o", now, source, mp); err != nil {
		_ = os.Remove(mp)
		_ = os.Remove(cred)
		if ctx.Err() != nil {
			return failed("连接网络存储超时：30 秒内没有连上服务器，请检查地址和网络", out, errors.New("mount: no answer within 30s"))
		}
		return failed("连接网络存储失败：请检查地址、共享名称和账号密码", out, err)
	}
	if n.Persist {
		if err := h.setFstab(mp, fmt.Sprintf("%s %s %s %s,_netdev,nofail,x-systemd.mount-timeout=30s 0 0", escapeFstab(source), escapeFstab(mp), fstype, opts)); err != nil {
			return err
		}
	}
	saved := *n
	saved.Password, saved.Server, saved.Share, saved.Mounted = "", server, share, false
	return h.update(func(s *state) error { s.NetMounts = append(s.NetMounts, saved); return nil })
}

func (h *Helper) removeNet(ctx context.Context, name string) error {
	mp, err := h.mountpoint(name)
	if err != nil {
		return err
	}
	if h.mounts()[mp] {
		if out, err := h.Run(ctx, nil, "umount", mp); err != nil {
			return failed("断开失败：可能还有程序在使用其中的文件", out, err)
		}
	}
	_ = os.Remove(mp)
	_ = os.Remove(filepath.Join(h.CredDir, name))
	if err := h.setFstab(mp, ""); err != nil {
		return err
	}
	return h.update(func(s *state) error {
		kept := s.NetMounts[:0]
		for _, m := range s.NetMounts {
			if m.Name != name {
				kept = append(kept, m)
			}
		}
		s.NetMounts = kept
		return nil
	})
}
