package dockercli

import "testing"

func TestParseDFOutput(t *testing.T) {
	t.Run("real busybox df -Pk from the sidecar image", func(t *testing.T) {
		out := "Filesystem           1024-blocks    Used Available Capacity Mounted on\n" +
			"overlay              243937628 197967884  33505568  86% /dockback-hostfs\n"
		got, err := parseDFOutput(out, "/dockback-hostfs")
		if err != nil {
			t.Fatal(err)
		}
		if got.FreeBytes != 33505568*1024 {
			t.Errorf("free = %d, want %d", got.FreeBytes, int64(33505568)*1024)
		}
		if got.TotalBytes != 243937628*1024 {
			t.Errorf("total = %d", got.TotalBytes)
		}
		if got.Filesystem != "overlay" {
			t.Errorf("filesystem = %q", got.Filesystem)
		}
		// The probe's own mount prefix must not leak into what the operator sees.
		if got.MountPoint != "/" {
			t.Errorf("mount point = %q, want / — the host's path, not the sidecar's view", got.MountPoint)
		}
	})

	t.Run("the probe prefix is stripped from a nested mount point", func(t *testing.T) {
		out := "Filesystem 1024-blocks Used Available Capacity Mounted on\n" +
			"/dev/sdb1 2147483648 10485760 2136997888 1% /dockback-hostfs/mnt/data\n"
		got, err := parseDFOutput(out, "/dockback-hostfs")
		if err != nil {
			t.Fatal(err)
		}
		if got.MountPoint != "/mnt/data" {
			t.Errorf("mount point = %q, want /mnt/data", got.MountPoint)
		}
	})

	t.Run("a device name containing spaces still parses", func(t *testing.T) {
		// An NFS or CIFS export routinely has one. Counting fields from the left
		// would mis-assign every number on exactly the hosts this feature is for.
		out := "Filesystem 1024-blocks Used Available Capacity Mounted on\n" +
			"//nas/my share 1000000 400000 600000 40% /mnt/nas\n"
		got, err := parseDFOutput(out, "")
		if err != nil {
			t.Fatal(err)
		}
		if got.FreeBytes != 600000*1024 {
			t.Errorf("free = %d, want %d", got.FreeBytes, int64(600000)*1024)
		}
		if got.Filesystem != "//nas/my share" {
			t.Errorf("filesystem = %q, want the whole name", got.Filesystem)
		}
		if got.MountPoint != "/mnt/nas" {
			t.Errorf("mount point = %q", got.MountPoint)
		}
	})

	t.Run("a filesystem that reports no figure is an error, never zero", func(t *testing.T) {
		// Zero would read as "full" and refuse every restore onto it; treating it
		// as unknown lets the caller proceed without a check it could not make.
		out := "Filesystem 1024-blocks Used Available Capacity Mounted on\n" +
			"tmpfs - - - - /mnt/x\n"
		if _, err := parseDFOutput(out, ""); err == nil {
			t.Error("want an error for an unreportable filesystem")
		}
	})

	t.Run("no usable line", func(t *testing.T) {
		for _, out := range []string{"", "df: /nope: No such file or directory\n",
			"Filesystem 1024-blocks Used Available Capacity Mounted on\n"} {
			if _, err := parseDFOutput(out, ""); err == nil {
				t.Errorf("want an error for %q", out)
			}
		}
	})
}
