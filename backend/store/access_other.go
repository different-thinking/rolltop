//go:build !linux

// File overview: Filesystem identification on platforms without the Linux
// superblock numbers. Development machines run on local disks, and the
// deployment target is Linux, so nothing is detected here.

package store

// DetectFilesystem reports an unidentified local filesystem.
func DetectFilesystem(string) FilesystemReport {
	return FilesystemReport{Name: "unknown", SharedMemorySafe: true}
}
