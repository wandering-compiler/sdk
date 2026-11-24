//go:build dockertest

package mysql_test

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// containerIP is a throwaway container's address on its docker network.
//
// The engine publishes NO host port: a published port (-P picks one at
// random) lands outside the host-port range a shared box gives each
// workspace. The container's own address is reachable from the host on Linux
// and from the lane's --network host test containers.
func containerIP(ctx context.Context, t *testing.T, id string) string {
	t.Helper()
	out, err := exec.CommandContext(ctx, "docker", "inspect", "-f",
		"{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}", id).Output()
	if err != nil {
		t.Fatalf("docker inspect %s: %v", id, err)
	}
	ips := strings.Fields(string(out))
	if len(ips) == 0 {
		t.Fatalf("container %s has no network address", id)
	}
	return ips[0]
}
