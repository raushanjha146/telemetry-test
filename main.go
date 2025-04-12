package main

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/mem"
	"gopkg.in/yaml.v3"
)

const subnet = "192.168.1."
const cfgPath = "config.yaml"

type DeviceTypeRule struct {
	Type             string   `yaml:"type"`
	MACPrefixes      []string `yaml:"mac_prefixes"`
	HostnameKeywords []string `yaml:"hostname_keywords"`
}

type Config struct {
	DeviceTypes []DeviceTypeRule `yaml:"device_types"`
}

var (
	cpuUsage = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "macbook_cpu_usage_percent",
		Help: "CPU usage percentage on MacBook",
	})

	memoryUsage = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "macbook_memory_usage_percent",
		Help: "Memory usage percentage on MacBook",
	})

	totalMemory = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "macbook_memory_total_bytes",
		Help: "Total memory on MacBook in bytes",
	})

	usedMemory = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "macbook_memory_used_bytes",
		Help: "Used memory on MacBook in bytes",
	})

	deviceDetails = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "wifi_connected_devices",
			Help: "Connected devices on the local network",
		},
		[]string{"ip", "mac", "hostname", "device_type"},
	)
)

func init() {
	prometheus.MustRegister(cpuUsage)
	prometheus.MustRegister(memoryUsage)
	prometheus.MustRegister(totalMemory)
	prometheus.MustRegister(usedMemory)
	prometheus.MustRegister(deviceDetails)
}

func ping(ip string, wg *sync.WaitGroup) {
	defer wg.Done()
	_ = exec.Command("ping", "-c", "1", "-W", "1", ip).Run()
}

func getARPTable() map[string]string {
	out, err := exec.Command("arp", "-a").Output()
	if err != nil {
		log.Println("Error getting ARP table:", err)
		return nil
	}

	lines := strings.Split(string(out), "\n")
	result := make(map[string]string)
	for _, line := range lines {
		parts := strings.Fields(line)
		if len(parts) >= 4 {
			ip := strings.Trim(parts[1], "()")
			mac := parts[3]
			result[ip] = mac
		}
	}
	return result
}

func resolveHostname(ip string) (string, error) {
	// Run `arp -a`
	cmd := exec.Command("arp", "-a")
	var out bytes.Buffer
	cmd.Stdout = &out

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to run arp: %v", err)
	}

	lines := strings.Split(out.String(), "\n")
	for _, line := range lines {
		if strings.Contains(line, ip) {
			// Example line: ? (192.168.1.5) at 8:xx:xx:xx:xx on en0 ifscope [ethernet]
			parts := strings.Fields(line)
			if len(parts) > 0 {
				if parts[0] != "?" {
					return parts[0], nil // parts[0] is the hostname
				} else {
					return "<unknown>", nil
				}
			}
		}
	}

	return "", fmt.Errorf("IP not found in ARP table")
}

func detectDeviceType(mac, hostname, configPath string) (string, error) {
	// Basic MAC OUI checks
	/* if strings.HasPrefix(mac, "fc:fb:fb") || strings.HasPrefix(mac, "ac:bc:32") {
		return "apple"
	} else if strings.HasPrefix(mac, "00:1a:11") || strings.HasPrefix(mac, "d0:37:45") {
		return "mobile"
	} else if strings.HasPrefix(mac, "3c:5a:b4") || strings.HasPrefix(mac, "28:d2:44") {
		return "windows"
	}

	// Heuristic hostname checks
	hostname = strings.ToLower(hostname)
	switch {
	case strings.Contains(hostname, "android"):
		return "mobile"
	case strings.Contains(hostname, "iphone"), strings.Contains(hostname, "ipad"), strings.Contains(hostname, "mac"):
		return "apple"
	case strings.Contains(hostname, "desktop"), strings.Contains(hostname, "win"):
		return "windows"
	default:
		return "unknown"
	} */
	data, err := os.ReadFile(configPath)
	if err != nil {
		return "unknown", err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return "unknown", err
	}
	mac = strings.ToLower(mac)
	hostname = strings.ToLower(hostname)
	for _, rule := range cfg.DeviceTypes {
		for _, prefix := range rule.MACPrefixes {
			if strings.HasPrefix(mac, prefix) {
				return rule.Type, nil
			}
		}
		for _, keyword := range rule.HostnameKeywords {
			if strings.Contains(hostname, keyword) {
				return rule.Type, nil
			}
		}
	}
	return "unknown", nil
}

func scanAndUpdateMetrics() {
	deviceDetails.Reset()

	var wg sync.WaitGroup
	for i := 1; i <= 254; i++ {
		ip := fmt.Sprintf("%s%d", subnet, i)
		wg.Add(1)
		go ping(ip, &wg)
	}
	wg.Wait()
	time.Sleep(1 * time.Second)

	arpTable := getARPTable()
	for ip, mac := range arpTable {
		hostname, err := resolveHostname(ip)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
		}
		deviceType, err := detectDeviceType(mac, hostname, cfgPath)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
		}
		//fmt.Println("ip : ", ip, "mac : ",mac,"hostname : ", hostname, "deviceType : ",deviceType)
		deviceDetails.WithLabelValues(ip, mac, hostname, deviceType).Set(1)
	}
}

func recordMetrics() {
	go func() {
		for {
			// CPU
			percent, err := cpu.Percent(0, false)
			if err == nil && len(percent) > 0 {
				cpuUsage.Set(percent[0])
			}

			// Memory
			v, err := mem.VirtualMemory()
			if err == nil {
				memoryUsage.Set(v.UsedPercent)
				totalMemory.Set(float64(v.Total))
				usedMemory.Set(float64(v.Used))
			}

			time.Sleep(5 * time.Second)
		}
	}()
}

func main() {
	recordMetrics()
	go func() {
		for {
			scanAndUpdateMetrics()
			time.Sleep(30 * time.Second) // Re-scan every 30 seconds
		}
	}()

	http.Handle("/metrics", promhttp.Handler())

	log.Println("Starting metrics server at :2112/metrics")
	log.Fatal(http.ListenAndServe(":2112", nil))
}
