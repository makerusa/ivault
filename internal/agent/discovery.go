package agent

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/grandcat/zeroconf"
)

type DiscoveredDevice struct {
	Name      string    `json:"name"`
	IP        string    `json:"ip"`
	Services  []string  `json:"services"`  // ["smb", "nfs", "afp"]
	Discovery string    `json:"discovery"` // "mdns", "scan"
	LastSeen  time.Time `json:"-"`
}

type Discovery struct {
	mu      sync.RWMutex
	devices map[string]*DiscoveredDevice // Key is IP
}

var GlobalDiscovery = &Discovery{
	devices: make(map[string]*DiscoveredDevice),
}

// StartDiscovery starts the background mDNS listener.
func (d *Discovery) Start(ctx context.Context) {
	log.Println("discovery: starting background mDNS listener")
	
	// Scan immediately on start
	go d.scanMDNS(ctx)

	// Periodic scan every 15 minutes
	ticker := time.NewTicker(15 * time.Minute)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				d.scanMDNS(ctx)
			}
		}
	}()
}

// GetDevices returns the current list of discovered devices.
func (d *Discovery) GetDevices() []DiscoveredDevice {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var list []DiscoveredDevice
	// Clean up devices not seen for more than 24 hours
	cutoff := time.Now().Add(-24 * time.Hour)
	
	for ip, dev := range d.devices {
		if dev.LastSeen.Before(cutoff) {
			delete(d.devices, ip)
			continue
		}
		list = append(list, *dev)
	}
	return list
}

func (d *Discovery) scanMDNS(ctx context.Context) {
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		log.Printf("discovery: failed to initialize resolver: %v", err)
		return
	}

	// We look for SMB and NFS services
	services := []string{"_smb._tcp", "_nfs._tcp", "_device-info._tcp"}
	
	for _, svc := range services {
		entries := make(chan *zeroconf.ServiceEntry)
		go func() {
			err = resolver.Browse(ctx, svc, "local.", entries)
			if err != nil {
				log.Printf("discovery: browse error for %s: %v", svc, err)
			}
		}()

		// Collect results for 5 seconds
		timeout := time.After(5 * time.Second)
	loop:
		for {
			select {
			case entry := <-entries:
				if entry == nil {
					break loop
				}
				d.processEntry(entry, svc)
			case <-timeout:
				break loop
			case <-ctx.Done():
				return
			}
		}
	}
}

func (d *Discovery) processEntry(entry *zeroconf.ServiceEntry, svc string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	ip := ""
	if len(entry.AddrIPv4) > 0 {
		ip = entry.AddrIPv4[0].String()
	} else if len(entry.AddrIPv6) > 0 {
		ip = entry.AddrIPv6[0].String()
	}

	if ip == "" {
		return
	}

	dev, ok := d.devices[ip]
	if !ok {
		dev = &DiscoveredDevice{
			Name:      entry.Instance,
			IP:        ip,
			Services:  []string{},
			Discovery: "mdns",
		}
		d.devices[ip] = dev
	}

	dev.LastSeen = time.Now()
	
	// Add service if not already present
	svcName := ""
	switch svc {
	case "_smb._tcp":
		svcName = "smb"
	case "_nfs._tcp":
		svcName = "nfs"
	}

	if svcName != "" {
		found := false
		for _, s := range dev.Services {
			if s == svcName {
				found = true
				break
			}
		}
		if !found {
			dev.Services = append(dev.Services, svcName)
		}
	}
}

// TriggerDeepScan performs a fast TCP port scan on common file sharing ports
// across the local subnet.
func (d *Discovery) TriggerDeepScan(ctx context.Context) {
	log.Println("discovery: starting deep subnet scan")
	
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return
	}

	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				go d.scanSubnet(ctx, ipnet)
			}
		}
	}
}

func (d *Discovery) scanSubnet(ctx context.Context, ipnet *net.IPNet) {
	// Simple scanner for Port 445 (SMB)
	baseIP := ipnet.IP.Mask(ipnet.Mask)
	
	// We only scan /24 subnets for safety/speed
	ones, bits := ipnet.Mask.Size()
	if ones < 24 {
		log.Printf("discovery: skipping large subnet %v", ipnet)
		return
	}

	var wg sync.WaitGroup
	for i := 1; i < 255; i++ {
		target := make(net.IP, len(baseIP))
		copy(target, baseIP)
		target[3] = byte(i)
		
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			
			// Check SMB port
			conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, "445"), 1*time.Second)
			if err == nil {
				conn.Close()
				d.mu.Lock()
				if _, ok := d.devices[ip]; !ok {
					d.devices[ip] = &DiscoveredDevice{
						Name:      "Device at " + ip,
						IP:        ip,
						Services:  []string{"smb"},
						Discovery: "scan",
						LastSeen:  time.Now(),
					}
				} else {
					// Update services if already found via mDNS but maybe mDNS didn't list SMB
					dev := d.devices[ip]
					dev.LastSeen = time.Now()
					found := false
					for _, s := range dev.Services {
						if s == "smb" { found = true; break }
					}
					if !found { dev.Services = append(dev.Services, "smb") }
				}
				d.mu.Unlock()
			}
		}(target.String())
	}
	wg.Wait()
	log.Println("discovery: deep scan complete")
}
