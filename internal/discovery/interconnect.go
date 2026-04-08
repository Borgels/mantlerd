package discovery

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/Borgels/mantlerd/internal/types"
)

var (
	netInterfacesFn  = net.Interfaces
	interfaceAddrsFn = func(iface net.Interface) ([]net.Addr, error) { return iface.Addrs() }
	readFileFn       = os.ReadFile
	execCommandFn    = exec.Command
	userHomeDirFn    = os.UserHomeDir
)

var ibdev2NetdevLinePattern = regexp.MustCompile(`^\s*([^\s]+)\s+port\s+\d+\s+==>\s+([^\s]+)\s+\((Up|Down)\)\s*$`)

// CollectInterconnect gathers network/fabric observations for cluster detection.
func CollectInterconnect() *types.InterconnectReport {
	rdmaDevices := collectRdmaDevices()
	rdmaByNetdev := map[string]string{}
	for _, dev := range rdmaDevices {
		if strings.TrimSpace(dev.Netdev) == "" {
			continue
		}
		rdmaByNetdev[strings.TrimSpace(dev.Netdev)] = strings.ToLower(strings.TrimSpace(dev.Type))
	}

	links := collectHighSpeedLinks(rdmaByNetdev)
	if len(links) == 0 && len(rdmaDevices) == 0 {
		return nil
	}

	peerIPs := make([]string, 0, len(links))
	for _, link := range links {
		if strings.TrimSpace(link.PeerAddress) != "" {
			peerIPs = append(peerIPs, strings.TrimSpace(link.PeerAddress))
		}
	}

	report := &types.InterconnectReport{
		Links: links,
	}
	if len(rdmaDevices) > 0 {
		report.RdmaDevices = rdmaDevices
	}
	if peers := collectPeerHostnames(peerIPs); len(peers) > 0 {
		report.PeerHostnames = peers
	}
	if fabricID := readFabricID(); fabricID != "" {
		report.FabricID = fabricID
	}

	return report
}

func collectHighSpeedLinks(rdmaByNetdev map[string]string) []types.NetworkLink {
	interfaces, err := netInterfacesFn()
	if err != nil {
		return nil
	}

	links := make([]types.NetworkLink, 0)
	for _, iface := range interfaces {
		name := strings.TrimSpace(iface.Name)
		if !includeInterface(iface, name) {
			continue
		}
		addrs, err := interfaceAddrsFn(iface)
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet == nil || ipNet.IP == nil {
				continue
			}
			ipv4 := ipNet.IP.To4()
			if ipv4 == nil || ipv4.IsLoopback() {
				continue
			}
			localAddress := ipv4.String()
			subnet := ipNet.String()
			linkType := classifyLinkType(name, rdmaByNetdev[name])
			link := types.NetworkLink{
				Interface:    name,
				Type:         linkType,
				State:        "up",
				LocalAddress: localAddress,
				Subnet:       subnet,
				MTU:          iface.MTU,
			}
			if speed := readInterfaceSpeed(name); speed != "" {
				link.Speed = speed
			}
			if linkType == "qsfp" || linkType == "infiniband" {
				if peer := findPeerAddress(name, ipNet, localAddress); peer != "" {
					link.PeerAddress = peer
				}
			}
			links = append(links, link)
		}
	}
	return dedupeLinks(links)
}

func includeInterface(iface net.Interface, name string) bool {
	if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
		return false
	}
	lower := strings.ToLower(name)
	if lower == "" {
		return false
	}
	if strings.HasPrefix(lower, "lo") ||
		strings.HasPrefix(lower, "docker") ||
		strings.HasPrefix(lower, "veth") ||
		strings.HasPrefix(lower, "br-") ||
		strings.HasPrefix(lower, "virbr") ||
		strings.HasPrefix(lower, "zt") ||
		strings.HasPrefix(lower, "tailscale") {
		return false
	}
	return true
}

func classifyLinkType(interfaceName string, rdmaType string) string {
	lower := strings.ToLower(strings.TrimSpace(interfaceName))
	switch {
	case strings.HasPrefix(lower, "ib"):
		return "infiniband"
	case isLikelyQsfpInterface(lower):
		return "qsfp"
	case rdmaType == "infiniband":
		return "infiniband"
	case rdmaType == "roce":
		return "qsfp"
	default:
		return "ethernet"
	}
}

func isLikelyQsfpInterface(name string) bool {
	if strings.Contains(name, "np0") || strings.Contains(name, "np1") {
		return true
	}
	if strings.HasPrefix(name, "enp1s0f") || strings.HasPrefix(name, "enp2p1s0f") {
		return true
	}
	return false
}

func readInterfaceSpeed(interfaceName string) string {
	path := filepath.Join("/sys/class/net", interfaceName, "speed")
	data, err := readFileFn(path)
	if err != nil {
		return ""
	}
	raw := strings.TrimSpace(string(data))
	if raw == "" {
		return ""
	}
	mbps, err := strconv.Atoi(raw)
	if err != nil || mbps <= 0 {
		return ""
	}
	gbps := float64(mbps) / 1000.0
	if gbps >= 1 {
		if gbps == float64(int64(gbps)) {
			return fmt.Sprintf("%dGb/s", int64(gbps))
		}
		return fmt.Sprintf("%.1fGb/s", gbps)
	}
	return fmt.Sprintf("%dMb/s", mbps)
}

func findPeerAddress(interfaceName string, subnet *net.IPNet, localAddress string) string {
	candidates := parseARPTable(interfaceName, subnet, localAddress)
	if len(candidates) == 1 {
		return candidates[0]
	}
	neighCandidates := parseIPNeigh(interfaceName, subnet, localAddress)
	if len(neighCandidates) == 1 {
		return neighCandidates[0]
	}
	return ""
}

func parseARPTable(interfaceName string, subnet *net.IPNet, localAddress string) []string {
	data, err := readFileFn("/proc/net/arp")
	if err != nil {
		return nil
	}
	return parseARPEntries(data, interfaceName, subnet, localAddress)
}

func parseARPEntries(data []byte, interfaceName string, subnet *net.IPNet, localAddress string) []string {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	first := true
	candidates := []string{}
	seen := map[string]struct{}{}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if first {
			first = false
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		ip := fields[0]
		iface := fields[5]
		if iface != interfaceName || ip == localAddress {
			continue
		}
		parsed := net.ParseIP(ip)
		if parsed == nil || parsed.To4() == nil {
			continue
		}
		if subnet != nil && !subnet.Contains(parsed) {
			continue
		}
		if _, exists := seen[ip]; exists {
			continue
		}
		seen[ip] = struct{}{}
		candidates = append(candidates, ip)
	}
	slices.Sort(candidates)
	return candidates
}

func parseIPNeigh(interfaceName string, subnet *net.IPNet, localAddress string) []string {
	output, err := execCommandFn("ip", "neigh", "show", "dev", interfaceName).Output()
	if err != nil {
		return nil
	}
	scanner := bufio.NewScanner(bytes.NewReader(output))
	candidates := []string{}
	seen := map[string]struct{}{}
	for scanner.Scan() {
		fields := strings.Fields(strings.TrimSpace(scanner.Text()))
		if len(fields) < 2 {
			continue
		}
		ip := fields[0]
		if ip == localAddress {
			continue
		}
		parsed := net.ParseIP(ip)
		if parsed == nil || parsed.To4() == nil {
			continue
		}
		if subnet != nil && !subnet.Contains(parsed) {
			continue
		}
		state := strings.ToUpper(fields[len(fields)-1])
		if state == "FAILED" || state == "INCOMPLETE" {
			continue
		}
		if _, exists := seen[ip]; exists {
			continue
		}
		seen[ip] = struct{}{}
		candidates = append(candidates, ip)
	}
	slices.Sort(candidates)
	return candidates
}

func collectRdmaDevices() []types.RdmaDevice {
	output, err := execCommandFn("ibdev2netdev").Output()
	if err != nil {
		return nil
	}
	return parseIbdev2NetdevOutput(output)
}

func parseIbdev2NetdevOutput(output []byte) []types.RdmaDevice {
	scanner := bufio.NewScanner(bytes.NewReader(output))
	devices := make([]types.RdmaDevice, 0)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		match := ibdev2NetdevLinePattern.FindStringSubmatch(line)
		if len(match) != 4 {
			continue
		}
		name := strings.TrimSpace(match[1])
		netdev := strings.TrimSpace(match[2])
		stateRaw := strings.ToLower(strings.TrimSpace(match[3]))
		state := "inactive"
		if stateRaw == "up" {
			state = "active"
		}
		deviceType := "other"
		lowerName := strings.ToLower(name)
		if strings.HasPrefix(lowerName, "roce") {
			deviceType = "roce"
		} else if strings.HasPrefix(lowerName, "ib") {
			deviceType = "infiniband"
		}
		devices = append(devices, types.RdmaDevice{
			Name:   name,
			Type:   deviceType,
			State:  state,
			Netdev: netdev,
		})
	}
	return devices
}

func collectPeerHostnames(peerIPs []string) []string {
	peerSet := map[string]struct{}{}
	for _, ip := range peerIPs {
		trimmed := strings.TrimSpace(ip)
		if trimmed != "" {
			peerSet[trimmed] = struct{}{}
		}
	}
	if len(peerSet) == 0 {
		return nil
	}

	hostnames := map[string]struct{}{}
	addFromKnownHosts(peerSet, hostnames)
	addFromAvahi(peerSet, hostnames)

	if len(hostnames) == 0 {
		return nil
	}
	result := make([]string, 0, len(hostnames))
	for host := range hostnames {
		result = append(result, host)
	}
	slices.Sort(result)
	return result
}

func addFromKnownHosts(peerSet map[string]struct{}, out map[string]struct{}) {
	home, err := userHomeDirFn()
	if err != nil || strings.TrimSpace(home) == "" {
		return
	}
	path := filepath.Join(home, ".ssh", "known_hosts")
	data, err := readFileFn(path)
	if err != nil {
		return
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		hosts := strings.Split(fields[0], ",")
		normalizedHosts := make([]string, 0, len(hosts))
		matchesPeerIP := false
		for _, host := range hosts {
			host = strings.TrimSpace(host)
			if host == "" || strings.HasPrefix(host, "|") {
				continue
			}
			normalized := strings.Trim(host, "[]")
			if idx := strings.Index(normalized, "]:"); idx > 0 && strings.HasPrefix(host, "[") {
				normalized = normalized[:idx]
			}
			normalizedHosts = append(normalizedHosts, normalized)
			if _, ok := peerSet[normalized]; ok {
				matchesPeerIP = true
			}
		}
		if !matchesPeerIP {
			continue
		}
		for _, host := range normalizedHosts {
			if net.ParseIP(host) != nil {
				continue
			}
			out[host] = struct{}{}
		}
	}
}

func addFromAvahi(peerSet map[string]struct{}, out map[string]struct{}) {
	for ip := range peerSet {
		cmd := execCommandFn("avahi-resolve", "--address", ip)
		raw, err := cmd.Output()
		if err != nil {
			continue
		}
		line := strings.TrimSpace(string(raw))
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		host := strings.TrimSpace(fields[1])
		if host != "" {
			out[host] = struct{}{}
		}
	}
}

func readFabricID() string {
	paths := []string{"/etc/mantler/fabric.id"}
	if home, err := userHomeDirFn(); err == nil && strings.TrimSpace(home) != "" {
		paths = append(paths, filepath.Join(home, ".mantler", "fabric.id"))
	}
	for _, path := range paths {
		data, err := readFileFn(path)
		if err != nil {
			continue
		}
		if value := strings.TrimSpace(string(data)); value != "" {
			return value
		}
	}
	return ""
}

func dedupeLinks(links []types.NetworkLink) []types.NetworkLink {
	if len(links) <= 1 {
		return links
	}
	seen := map[string]struct{}{}
	result := make([]types.NetworkLink, 0, len(links))
	for _, link := range links {
		key := strings.Join([]string{
			link.Interface,
			link.Type,
			link.LocalAddress,
			link.PeerAddress,
			link.Subnet,
		}, "|")
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, link)
	}
	return result
}
