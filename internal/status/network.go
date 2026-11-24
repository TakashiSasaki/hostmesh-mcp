package status

import (
	"net"
	"slices"
)

func networkInterfaces(sectionErrors map[string]string) ([]Interface, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]Interface, 0, len(interfaces))
	for _, iface := range interfaces {
		row := Interface{
			Index: iface.Index, Name: iface.Name, MTU: iface.MTU,
			Up: iface.Flags&net.FlagUp != 0, Running: iface.Flags&net.FlagRunning != 0,
			Loopback: iface.Flags&net.FlagLoopback != 0, Addresses: []string{},
		}
		addrs, err := iface.Addrs()
		if err != nil {
			sectionErrors["network:"+iface.Name] = err.Error()
		}
		for _, addr := range addrs {
			row.Addresses = append(row.Addresses, addr.String())
		}
		slices.Sort(row.Addresses)
		out = append(out, row)
	}
	return out, nil
}
