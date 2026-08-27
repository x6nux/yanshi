package securityverify

import (
	"os"
	"path/filepath"
	"testing"
)

// TestS10_StorageDestructionMatrix pins the tier of the command family the
// canary run found unguarded: programs that destroy the storage rather than
// the files. Before internal/guard/storage.go every "want structural" row here
// returned Allow under a wide shell profile, and Prompt (which yolo skips)
// under the shipped one.
//
// The controls are load-bearing. `dd if=x of=./disk.img` and `mkfs.ext4
// ./loopimage` are how disk images are actually built, and a gate that refuses
// them is a gate an operator disables.
func TestS10_StorageDestructionMatrix(t *testing.T) {
	home := t.TempDir()
	if r, err := filepath.EvalSymlinks(home); err == nil {
		home = r
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	work := filepath.Join(home, "proj")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		cmd  string
		want tier
		why  string
	}{
		// Unrecoverable storage destruction: structural, yolo included.
		{`dd if=/dev/zero of=/dev/disk0`, tierStructural, "the command the canary caught executing"},
		{`dd if=/dev/zero of=/dev/sda bs=1M`, tierStructural, ""},
		{`dd if=/dev/urandom of=/dev/rdisk0`, tierStructural, ""},
		{`dd of=//./PhysicalDrive0 if=x.img`, tierStructural, "windows raw device namespace"},
		{`mkfs.ext4 /dev/sda1`, tierStructural, "suffixed mkfs spelling"},
		{`mkfs -t ext4 /dev/sda1`, tierStructural, ""},
		{`newfs /dev/disk0s2`, tierStructural, ""},
		{`mkswap /dev/sda2`, tierStructural, ""},
		{`wipefs -a /dev/sda`, tierStructural, ""},
		{`blkdiscard /dev/sda`, tierStructural, ""},
		{`sgdisk --zap-all /dev/sda`, tierStructural, "scripted partition edit"},
		{`diskutil eraseDisk JHFS+ Blank /dev/disk0`, tierStructural, "verb, not operand position"},
		{`diskutil zeroDisk /dev/disk0`, tierStructural, ""},
		{`shred -n 3 /dev/sda`, tierStructural, "shred on a device is not a file deletion"},
		{`format C: /q`, tierStructural, "whole windows volume"},
		{`badblocks -w /dev/sda`, tierStructural, "destructive write test"},
		// The wrapper/ANSI-C machinery must apply here too, for free.
		{`bash -c "dd if=/dev/zero of=/dev/disk0"`, tierStructural, "wrapper unwrapping is inherited"},
		{`sh -c 'mkfs.ext4 /dev/sda1'`, tierStructural, ""},
		// find that removes what it walks, rooted at /.
		{`find / -delete`, tierStructural, ""},
		{`find / -exec rm -rf {} +`, tierStructural, "the removal is in the exec payload"},

		// Reversible: prompts, and yolo still refuses an OutOfScope grade.
		{`chmod -R 000 /`, tierPrompt, "bricks a machine but is reversible"},
		{`chown -R nobody /`, tierPrompt, ""},

		// Controls: ordinary uses of the very same programs.
		{`dd if=ubuntu.iso of=./disk.img bs=4M`, tierAllow, "building a disk image"},
		{`dd if=/dev/zero of=testfile count=1`, tierAllow, "zero-filled test fixture"},
		{`dd if=/dev/zero of=/dev/null count=1`, tierAllow, "/dev/null is not storage"},
		{`mkfs.ext4 ./loopimage`, tierAllow, "formatting a regular file for loop mount"},
		{`parted -l`, tierAllow, "listing partitions is not editing them"},
		{`find . -name "*.tmp" -delete`, tierAllow, "routine cleanup"},
		{`chmod -R 755 ./scripts`, tierAllow, "in-workdir permission fix"},
		{`shred ./secret.txt`, tierAllow, "shredding a file in the workdir"},
	}

	for _, tc := range cases {
		t.Run(tc.cmd, func(t *testing.T) {
			got, reason := shellTier(t, tc.cmd, work)
			t.Logf("cmd=%s tier=%s reason=%s", tc.cmd, got, reason)
			if got != tc.want {
				t.Fatalf("tier mismatch for %q: got %s want %s (%s)\n  reason: %s",
					tc.cmd, got, tc.want, tc.why, reason)
			}
		})
	}
}
