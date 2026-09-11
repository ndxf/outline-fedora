package main

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
)

// chgrpSocket sets the group ownership of the socket to groupName.
// Called after Listen; requires the daemon to run as a user in that
// group or as root.
func chgrpSocket(path, groupName string) error {
	g, err := user.LookupGroup(groupName)
	if err != nil {
		return fmt.Errorf("lookup group %q: %w", groupName, err)
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return fmt.Errorf("parse gid %q: %w", g.Gid, err)
	}
	// -1 uid = leave alone.
	if err := os.Chown(path, -1, gid); err != nil {
		return fmt.Errorf("chown %s -> gid %d: %w", path, gid, err)
	}
	return nil
}
