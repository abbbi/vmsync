package libvirtsync

import "testing"

func TestTargetNvramPath(t *testing.T) {
	cases := []struct {
		name, src, sd, td, want string
	}{
		{"same name is untouched -- the default case",
			"/var/lib/libvirt/qemu/nvram/web01_VARS.fd", "web01", "web01",
			"/var/lib/libvirt/qemu/nvram/web01_VARS.fd"},
		{"libvirt's own layout, renamed for the target",
			"/var/lib/libvirt/qemu/nvram/web01_VARS.fd", "web01", "web01-dr",
			"/var/lib/libvirt/qemu/nvram/web01-dr_VARS.fd"},
		{"custom directory is preserved, filename still corrected",
			"/srv/uefi/web01.fd", "web01", "web01-dr",
			"/srv/uefi/web01-dr.fd"},
		{"filename not derived from the domain is left alone",
			"/srv/uefi/shared-vars.fd", "web01", "web01-dr",
			"/srv/uefi/shared-vars.fd"},
		{"no varstore at all", "", "web01", "web01-dr", ""},
		{"missing source name is not guessed at",
			"/var/lib/libvirt/qemu/nvram/web01_VARS.fd", "", "web01-dr",
			"/var/lib/libvirt/qemu/nvram/web01_VARS.fd"},
		{"missing target name is not guessed at",
			"/var/lib/libvirt/qemu/nvram/web01_VARS.fd", "web01", "",
			"/var/lib/libvirt/qemu/nvram/web01_VARS.fd"},
		{"a domain name appearing in the DIRECTORY is not rewritten",
			"/srv/web01/shared.fd", "web01", "web01-dr",
			"/srv/web01/shared.fd"},
		{"only the first occurrence in the basename",
			"/n/web01_web01_VARS.fd", "web01", "dr",
			"/n/dr_web01_VARS.fd"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := TargetNvramPath(c.src, c.sd, c.td); got != c.want {
				t.Errorf("TargetNvramPath(%q,%q,%q) = %q, want %q", c.src, c.sd, c.td, got, c.want)
			}
		})
	}
}

// The property that matters: a rewritten path must not still contain the
// source's name where a collision could happen.
func TestRewrittenPathCannotCollideWithTheSourceDomain(t *testing.T) {
	src := "/var/lib/libvirt/qemu/nvram/web01_VARS.fd"
	got := TargetNvramPath(src, "web01", "web01-dr")
	if got == src {
		t.Fatal("the path was not rewritten, so both domains would share one varstore")
	}
	if got == "/var/lib/libvirt/qemu/nvram/web01_VARS.fd" {
		t.Fatal("the rewritten path is still the source's")
	}
}
