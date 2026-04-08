package discovery

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestCollectHighSpeedLinksCollectsEthernetLink(t *testing.T) {
	origNetInterfaces := netInterfacesFn
	origInterfaceAddrs := interfaceAddrsFn
	origReadFile := readFileFn
	origExecCommand := execCommandFn
	t.Cleanup(func() {
		netInterfacesFn = origNetInterfaces
		interfaceAddrsFn = origInterfaceAddrs
		readFileFn = origReadFile
		execCommandFn = origExecCommand
	})

	netInterfacesFn = func() ([]net.Interface, error) {
		return []net.Interface{
			{Name: "eth42", Flags: net.FlagUp, MTU: 1500},
		}, nil
	}
	interfaceAddrsFn = func(iface net.Interface) ([]net.Addr, error) {
		return []net.Addr{
			&net.IPNet{IP: net.ParseIP("192.168.50.10"), Mask: net.CIDRMask(24, 32)},
		}, nil
	}
	readFileFn = func(path string) ([]byte, error) {
		if path == "/sys/class/net/eth42/speed" {
			return []byte("1000\n"), nil
		}
		return nil, os.ErrNotExist
	}
	execCommandFn = nil

	links := collectHighSpeedLinks(map[string]string{})
	if len(links) != 1 {
		t.Fatalf("expected exactly one link, got %d", len(links))
	}
	link := links[0]
	if link.Interface != "eth42" {
		t.Fatalf("expected interface eth42, got %q", link.Interface)
	}
	if link.Type != "ethernet" {
		t.Fatalf("expected ethernet type, got %q", link.Type)
	}
	if link.Speed != "1Gb/s" {
		t.Fatalf("expected speed 1Gb/s, got %q", link.Speed)
	}
	if link.LocalAddress != "192.168.50.10" {
		t.Fatalf("unexpected local address: %q", link.LocalAddress)
	}
	if link.Subnet != "192.168.50.10/24" {
		t.Fatalf("unexpected subnet: %q", link.Subnet)
	}
}

func TestParseIbdev2NetdevOutput(t *testing.T) {
	out := []byte(`
roceP2p1s0f1 port 1 ==> enP2p1s0f1np1 (Up)
ib0 port 1 ==> ib0 (Down)
`)
	devices := parseIbdev2NetdevOutput(out)
	if len(devices) != 2 {
		t.Fatalf("expected 2 rdma devices, got %d", len(devices))
	}
	if devices[0].Name != "roceP2p1s0f1" || devices[0].Type != "roce" || devices[0].State != "active" || devices[0].Netdev != "enP2p1s0f1np1" {
		t.Fatalf("unexpected first device: %+v", devices[0])
	}
	if devices[1].Name != "ib0" || devices[1].Type != "infiniband" || devices[1].State != "inactive" || devices[1].Netdev != "ib0" {
		t.Fatalf("unexpected second device: %+v", devices[1])
	}
}

func TestParseARPEntriesFindsSinglePeer(t *testing.T) {
	_, subnet, err := net.ParseCIDR("10.0.0.2/24")
	if err != nil {
		t.Fatalf("parse cidr: %v", err)
	}
	arp := []byte(`IP address       HW type     Flags       HW address            Mask     Device
10.0.0.2         0x1         0x2         aa:bb:cc:dd:ee:10     *        enp1s0f0np0
10.0.0.3         0x1         0x2         aa:bb:cc:dd:ee:11     *        enp1s0f0np0
`)
	peers := parseARPEntries(arp, "enp1s0f0np0", subnet, "10.0.0.2")
	if len(peers) != 1 || peers[0] != "10.0.0.3" {
		t.Fatalf("unexpected peers: %+v", peers)
	}
}

func TestReadFabricIDPrefersSystemThenUserFile(t *testing.T) {
	origReadFile := readFileFn
	origHome := userHomeDirFn
	t.Cleanup(func() {
		readFileFn = origReadFile
		userHomeDirFn = origHome
	})

	tmp := t.TempDir()
	userHomeDirFn = func() (string, error) { return tmp, nil }
	systemPath := "/etc/mantler/fabric.id"
	userPath := filepath.Join(tmp, ".mantler", "fabric.id")

	readFileFn = func(path string) ([]byte, error) {
		switch path {
		case systemPath:
			return []byte("fabric-system\n"), nil
		case userPath:
			return []byte("fabric-user\n"), nil
		default:
			return nil, os.ErrNotExist
		}
	}

	if got := readFabricID(); got != "fabric-system" {
		t.Fatalf("expected system fabric id, got %q", got)
	}

	readFileFn = func(path string) ([]byte, error) {
		switch path {
		case systemPath:
			return nil, os.ErrNotExist
		case userPath:
			return []byte("fabric-user\n"), nil
		default:
			return nil, os.ErrNotExist
		}
	}
	if got := readFabricID(); got != "fabric-user" {
		t.Fatalf("expected user fabric id, got %q", got)
	}
}
