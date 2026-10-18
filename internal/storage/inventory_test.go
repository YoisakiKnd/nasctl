package storage

import "testing"

// util-linux 2.39 style: numbers, booleans and a mountpoints array.
const lsblkNew = `{"blockdevices":[
 {"name":"loop0","kname":"loop0","path":"/dev/loop0","type":"loop","size":52711424,"fstype":"squashfs","mountpoints":["/snap/x"]},
 {"name":"sda","kname":"sda","path":"/dev/sda","type":"disk","size":85899345920,"fstype":null,"label":null,"uuid":null,"mountpoints":[null],"model":"QEMU HARDDISK","serial":"QM0001","vendor":"QEMU","tran":"sata","rm":false,"ro":false,
  "children":[
   {"name":"sda1","kname":"sda1","path":"/dev/sda1","type":"part","size":84825604096,"fstype":"ext4","label":"root","uuid":"aaaa","mountpoints":["/"],"fsavail":40000,"fsused":50000},
   {"name":"sda15","kname":"sda15","path":"/dev/sda15","type":"part","size":111149056,"fstype":"vfat","uuid":"BBBB","mountpoints":["/boot/efi"]}
  ]},
 {"name":"sdb","kname":"sdb","path":"/dev/sdb","type":"disk","size":2000398934016,"model":"WDC WD20EFRX","serial":"WD-123","tran":"usb","rm":true,"ro":false,
  "children":[{"name":"sdb1","kname":"sdb1","path":"/dev/sdb1","type":"part","size":2000397795328,"fstype":"ext4","label":"data","uuid":"cccc","mountpoints":["/media/nasctl/data"],"fsavail":1000,"fsused":2000}]},
 {"name":"mmcblk1boot0","kname":"mmcblk1boot0","type":"disk","size":4194304},
 {"name":"sr0","kname":"sr0","type":"rom","size":4194304}
]}`

// util-linux 2.33 style: every value a string and a single mountpoint.
const lsblkOld = `{"blockdevices":[
 {"name":"mmcblk1","kname":"mmcblk1","type":"disk","size":"7818182656","fstype":null,"mountpoint":null,"model":null,"tran":null,"rm":"0","ro":"0",
  "children":[{"name":"mmcblk1p1","kname":"mmcblk1p1","type":"part","size":"7700000000","fstype":"ext4","label":null,"uuid":"dddd","mountpoint":"/","fsavail":"1","fsused":"2"}]},
 {"name":"sda","kname":"sda","type":"disk","size":"500107862016","fstype":"exfat","label":"USB","uuid":"EEEE","mountpoint":null,"model":"Ultra","tran":"usb","rm":"1","ro":"0"}
]}`

func TestParseLsblkAcrossVersions(t *testing.T) {
	disks, err := ParseLsblk([]byte(lsblkNew))
	if err != nil || len(disks) != 2 {
		t.Fatal(disks, err)
	}
	sys, data := disks[0], disks[1]
	if sys.Name != "sda" || !sys.System || len(sys.Partitions) != 2 || !sys.Partitions[0].System || sys.Transport != "sata" {
		t.Fatalf("system disk %+v", sys)
	}
	if data.Name != "sdb" || data.System || !data.Removable || data.Partitions[0].Label != "data" || !data.Partitions[0].Managed || data.Partitions[0].Avail != 1000 {
		t.Fatalf("data disk %+v", data)
	}

	old, err := ParseLsblk([]byte(lsblkOld))
	if err != nil || len(old) != 2 {
		t.Fatal(old, err)
	}
	if old[0].Name != "mmcblk1" || !old[0].System || old[0].Transport != "mmc" || old[0].Path != "/dev/mmcblk1" || old[0].Size != 7818182656 {
		t.Fatalf("SD card %+v", old[0])
	}
	usb := old[1]
	if !usb.Removable || len(usb.Partitions) != 1 || usb.Partitions[0].FSType != "exfat" || usb.Partitions[0].Path != "/dev/sda" || len(usb.Partitions[0].Mountpoints) != 0 {
		t.Fatalf("whole-disk filesystem %+v", usb)
	}
}
