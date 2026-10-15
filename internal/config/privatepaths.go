package config

// StoragePrivateName is shared by config and the file kernel boundary. A root
// cannot be configured inside one of these namespaces, including through aliases.
func StoragePrivateName(name string) bool {
	switch name {
	case ".nasctl-trash", ".nasctl-staging", ".nasctl-copy", ".nasctl-write.lock", ".nasctl-backups":
		return true
	}
	return false
}
