package guard

import "strings"

// storage.go extends the destructive gate from "programs that delete files" to
// "programs that destroy the storage the files live on".
//
// WHY THIS FILE EXISTS. The gate in destructive.go is keyed on
// deletionPrograms — rm, rmdir, shred, del and friends — and everything else
// falls through to the profile. That is the right model for a gate whose job
// is to catch `rm -rf /`, and it is exactly wrong for `dd if=/dev/zero
// of=/dev/disk0`, which destroys strictly more than `rm -rf /` does (it takes
// the partition table with it) while containing no deletion program at all.
//
// This was not reasoned out; it was MEASURED. A verification run put a canary
// binary named `rm`/`dd`/`shred` ahead of the real ones on PATH, scripted a
// model to call shell_run with a list of destructive commands, and checked
// afterwards whether the canary had been invoked. Every `rm` spelling was
// refused before reaching a process. `dd if=/dev/zero of=/dev/disk0` ran, and
// the canary recorded its argv. The gate had been asserted correct by tier
// tables that only ever listed deletion programs — the table and the gate
// agreed with each other and both were narrower than the threat.
//
// REACHABILITY. Under the shipped `coding` profile these commands are a
// Prompt, because the shell allowlist does not list them. That is not
// protection: Prompt is precisely the tier yolo mode exists to skip, and the
// mode gate only hard-stops what ClassifyDestruction grades Catastrophic. So
// before this file, `/mode yolo` plus one model turn was enough to zero a
// disk, and the operator's only warning would have been the one they did not
// get. An operator with `shell: {policy: allowlist, patterns: ["*"]}` — a
// common local relaxation — got it at Allow with no mode change at all.
//
// TIER SPLIT, AND WHY IT IS NOT ONE TIER. Two families arrive here and they
// are graded differently on RECOVERABILITY, which is the same axis the rest of
// the gate already uses to separate Catastrophic from OutOfScope:
//
//   - Storage destruction (dd onto a device, mkfs, wipefs, blkdiscard,
//     diskutil eraseDisk, format) is unrecoverable and has no legitimate use
//     from inside a coding agent. Catastrophic: refused in every mode, yolo
//     included.
//   - Recursive ownership/permission changes on a system root (chmod -R 000 /)
//     brick a machine but are reversible, and "chown -R me ~" is a thing real
//     people really run to repair a broken install. Grading that unappealable
//     would be the kind of false positive that teaches operators to switch the
//     gate off. OutOfScope: prompts by default, and yolo still refuses it.
//
// The whole file hangs off classifyDestruction, so it inherits the wrapper and
// ANSI-C unwrapping already proven there: `bash -c $'\x64\x64 of=/dev/disk0'`
// is graded through the same path as the plain spelling, with no extra code.

// storageDestroyers are programs whose ordinary, non-erroneous operation
// destroys a block device or the filesystem on it. Membership is not "this
// program is dangerous" — nearly every program is, given an argument — but
// "the destruction is what the program is FOR", so a device operand is
// sufficient evidence of intent.
//
// Program words arrive base-named and lowercased from lexShellLite, and
// mkfs-family names carry a filesystem suffix (mkfs.ext4, newfs_hfs), so the
// suffixed spellings are matched by prefix in isStorageDestroyer rather than
// enumerated here.
var storageDestroyers = map[string]bool{
	"dd":         true, // of=<device> overwrites the raw device
	"mkfs":       true, // makes a new filesystem, discarding the old one
	"mke2fs":     true,
	"newfs":      true,
	"mkswap":     true,
	"wipefs":     true, // erases filesystem signatures
	"blkdiscard": true,
	"sgdisk":     true, // --zap-all destroys both GPT copies
	"gdisk":      true,
	"fdisk":      true,
	"parted":     true,
	"diskutil":   true, // macOS: eraseDisk / zeroDisk / partitionDisk
	"format":     true, // Windows
	"shred":      true, // also a deletionProgram; on a device it is unrecoverable
	"badblocks":  true, // -w is a destructive write test
	"truncate":   true, // -s 0 on a device zeroes its size; on a file, its contents
	// `tar` is here under a NARROWER rule than the rest of the table, applied in
	// isStorageDestruction: only its WRITING modes count. The membership rule
	// above ("the destruction is what the program is for") does not hold for an
	// archiver — `tar -xf /dev/st0` restores from a tape and must keep working —
	// but `tar -cf /dev/sda .` writes an archive over a raw block device, and
	// the create flag makes the intent unambiguous without a second reading.
	"tar": true,
}

// tarWritingModes are the tar operations that WRITE to the archive named by -f.
// Everything else (-x extract, -t list, -d diff) reads it.
var tarWritingModes = map[byte]bool{'c': true, 'r': true, 'u': true, 'A': true}

// mkfsPrefixes are the program-name prefixes whose suffixed spellings name the
// same tool: mkfs.ext4, mkfs.xfs, newfs_hfs, newfs_apfs.
var mkfsPrefixes = []string{"mkfs.", "newfs_", "mkfs_"}

// isStorageDestroyer reports whether the program word names a storage
// destroyer, accounting for the filesystem-suffixed spellings.
func isStorageDestroyer(program string) bool {
	if storageDestroyers[program] {
		return true
	}
	for _, p := range mkfsPrefixes {
		if strings.HasPrefix(program, p) {
			return true
		}
	}
	return false
}

// safeDevices are /dev entries that are NOT storage: writing to them is
// ordinary and common (`dd if=x of=/dev/null`, `> /dev/stderr`). Without this
// set the gate would refuse the single most common legitimate dd invocation
// there is, and a gate that refuses the common case is a gate that gets
// removed.
var safeDevices = map[string]bool{
	"/dev/null": true, "/dev/zero": true, "/dev/full": true,
	"/dev/random": true, "/dev/urandom": true,
	"/dev/tty": true, "/dev/console": true,
	"/dev/stdin": true, "/dev/stdout": true, "/dev/stderr": true,
	"/dev/ptmx": true,
}

// safeDevicePrefixes are /dev subtrees that are likewise not storage.
var safeDevicePrefixes = []string{"/dev/fd/", "/dev/pts/", "/dev/tty"}

// isRawDevicePath reports whether a token names a raw block device — the thing
// that makes a storage destroyer's operand catastrophic rather than ordinary.
//
// The distinction this draws is the one that keeps the gate usable: `dd
// if=ubuntu.iso of=./disk.img` builds a disk image (a regular file) and must
// keep working, while `dd if=ubuntu.iso of=/dev/disk0` overwrites the machine.
// Only the operand's LOCATION separates them.
func isRawDevicePath(t string) bool {
	p := normLiteral(t)
	// Windows raw device namespace: \\.\PhysicalDrive0, \\.\C:
	if strings.HasPrefix(p, "//./") || strings.HasPrefix(p, "//?/") {
		return true
	}
	// A bare drive letter ("c:", "c:/") is a whole volume — `format C:`.
	if (len(p) == 2 || len(p) == 3) && p[1] == ':' && (len(p) == 2 || p[2] == '/') {
		return true
	}
	if !strings.HasPrefix(p, "/dev/") {
		return false
	}
	if safeDevices[p] {
		return false
	}
	for _, pre := range safeDevicePrefixes {
		if strings.HasPrefix(p, pre) {
			return false
		}
	}
	return true
}

// ddOutputIsRawDevice reports whether a dd invocation writes to a raw device.
//
// Only `of=` is inspected, and that asymmetry is deliberate: READING a disk
// (`dd if=/dev/disk0 of=./backup.img`) is how a backup is taken and destroys
// nothing. Treating both operands alike would refuse the recovery procedure
// and permit nothing extra.
func ddOutputIsRawDevice(args []string) bool {
	for _, a := range args {
		v, ok := strings.CutPrefix(strings.ToLower(strings.Trim(a, `"'`)), "of=")
		if !ok {
			continue
		}
		if isRawDevicePath(v) {
			return true
		}
	}
	return false
}

// diskutilDestructiveVerbs are the macOS diskutil subcommands that destroy
// data. diskutil's device operand moves around between subcommands
// (`eraseDisk JHFS+ Name disk0` puts it fourth), so the VERB is the reliable
// signal — and every verb here is destructive regardless of which device it is
// pointed at, which makes finding the operand unnecessary.
var diskutilDestructiveVerbs = map[string]bool{
	"erasedisk": true, "erasevolume": true, "zerodisk": true,
	"randomdisk": true, "securedisk": true, "reformat": true,
	"partitiondisk": true, "eraseoptical": true,
}

// isStorageDestruction reports whether a lexed command destroys storage.
//
// The rule is program-specific because the evidence differs per program: dd
// needs an of= operand, diskutil needs a destructive verb, and the mkfs family
// needs a device operand (so `mkfs.ext4 ./loopimage`, which formats a regular
// file used as a loop device, keeps working).
func isStorageDestruction(program string, args []string) bool {
	if !isStorageDestroyer(program) {
		return false
	}
	switch {
	case program == "dd":
		return ddOutputIsRawDevice(args)
	case program == "diskutil":
		for _, a := range args {
			if diskutilDestructiveVerbs[strings.ToLower(strings.Trim(a, `"'`))] {
				return true
			}
		}
		return false
	case program == "tar":
		return tarWritesArchive(args) && anyRawDeviceOperand(args)
	case program == "fdisk" || program == "gdisk" || program == "parted":
		// These are interactive partition editors: merely opening one is not
		// destruction, and refusing `parted -l` (a listing) would be a false
		// positive. Only the scripted, non-interactive forms act without a
		// further human step.
		if !hasScriptedPartitionFlag(args) {
			return false
		}
		return anyRawDeviceOperand(args)
	default:
		return anyRawDeviceOperand(args)
	}
}

// tarWritesArchive reports whether a tar invocation creates or appends to the
// archive, in either the clustered (`-cf`, `cf`) or long (`--create`) spelling.
func tarWritesArchive(args []string) bool {
	for i, a := range args {
		switch a {
		case "--create", "--append", "--update", "--concatenate", "--catenate":
			return true
		}
		if strings.HasPrefix(a, "--") {
			continue
		}
		// tar accepts its mode cluster with or without a dash, but ONLY as the
		// first word. Scanning every operand instead would find the `c` in a
		// file named `create.txt` and read an extraction as a creation.
		cluster, isFlag := strings.CutPrefix(a, "-")
		if !isFlag && i != 0 {
			continue
		}
		for j := 0; j < len(cluster); j++ {
			if tarWritingModes[cluster[j]] {
				return true
			}
		}
	}
	return false
}

// hasScriptedPartitionFlag reports whether a partition editor was invoked in a
// non-interactive mode that applies changes without another confirmation.
func hasScriptedPartitionFlag(args []string) bool {
	for _, a := range args {
		switch strings.ToLower(strings.Trim(a, `"'`)) {
		case "-s", "--script", "--zap-all", "-z", "--wipe", "--delete", "-d":
			return true
		}
	}
	return false
}

// anyRawDeviceOperand reports whether any argument names a raw device.
func anyRawDeviceOperand(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		if isRawDevicePath(a) {
			return true
		}
	}
	return false
}

// recursiveOwnershipPrograms change permissions or ownership over a tree.
// Pointed at a system root they render a machine unusable, but the change is
// reversible, which is why the caller grades them OutOfScope rather than
// Catastrophic. See the file header.
var recursiveOwnershipPrograms = map[string]bool{
	"chmod": true, "chown": true, "chgrp": true, "setfacl": true,
}

// isRecursiveOwnershipOnCatastrophicTarget reports whether a permission or
// ownership change is applied recursively to a system root, the home
// directory, or an ancestor of the working directory.
//
// It reuses isCatastrophicTarget rather than testing paths itself so that the
// spellings destructive.go already sees through — ~, $HOME, C:\Users\..\.. —
// are seen through here too. A second, hand-written path test would be the
// weaker one, and the weaker one is the one that gets bypassed.
func isRecursiveOwnershipOnCatastrophicTarget(program string, args []string, workdir string) bool {
	if !recursiveOwnershipPrograms[program] {
		return false
	}
	if !deleteIsRecursive(program, args) {
		return false
	}
	for _, t := range deleteTargets(args) {
		if isCatastrophicTarget(t, workdir) {
			return true
		}
	}
	return false
}

// findDeleteOnCatastrophicTarget reports whether a `find` invocation walks a
// system root, the home directory, or an ancestor of the working directory
// while carrying an action that removes what it finds.
//
// It deliberately does NOT reuse isCatastrophicTarget, even though the rest of
// this file does, because that predicate also grades "the workdir itself" as
// catastrophic — correct for `rm -rf .`, wrong for find. `.` is find's
// ORDINARY starting point, and the first control this gate was tested against,
// `find . -name '*.tmp' -delete`, is about as routine as cleanup gets. Reusing
// the shared predicate refused it. What actually makes a find catastrophic is
// the traversal ESCAPING the project, so that is what is tested: a system
// root, a home root, or an ancestor of the workdir.
func findDeleteOnCatastrophicTarget(program string, args []string, workdir string) bool {
	if program != "find" {
		return false
	}
	deletes := false
	for i, a := range args {
		switch strings.ToLower(a) {
		case "-delete":
			deletes = true
		case "-exec", "-execdir", "-ok", "-okdir":
			// `find / -exec rm -rf {} +` — the removal is in the payload.
			for _, p := range args[i+1:] {
				if deletionPrograms[strings.ToLower(basename(p))] {
					deletes = true
					break
				}
			}
		}
	}
	if !deletes {
		return false
	}
	// find's path operands precede its first predicate (which begins with "-").
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			break
		}
		if findRootEscapesProject(a, workdir) {
			return true
		}
	}
	return false
}

// findRootEscapesProject reports whether a find traversal root is a system
// root, the home directory, or an ancestor of the working directory.
//
// It does NOT consult catastrophicRoots, and that is the second half of the
// same correction described on findDeleteOnCatastrophicTarget. That table
// contains ".", "./" and "*" — right for `rm -rf .`, which erases the project,
// and wrong for `find . -delete`, where "." is simply where find starts. The
// only spellings tested here are ones that mean the traversal leaves the
// project entirely.
func findRootEscapesProject(t, workdir string) bool {
	resolved, ok := normalizePath(t, workdir)
	if !ok {
		return false
	}
	if resolvedIsCatastrophicRoot(resolved) {
		return true
	}
	if workdir == "" {
		return false
	}
	return isAncestorOf(resolved, cleanScope(workdir))
}

// basename strips any directory prefix from a program word, in the
// forward-slashed spelling normLiteral produces.
func basename(p string) string {
	p = strings.ReplaceAll(strings.Trim(p, `"'`), "\\", "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
