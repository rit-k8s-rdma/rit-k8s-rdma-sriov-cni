package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/cal8384/k8s-rdma-common/knapsack_pod_placement"
	"github.com/cal8384/k8s-rdma-common/rdma_hardware_info"

	"github.com/containernetworking/cni/pkg/ipam"
	"github.com/containernetworking/cni/pkg/ns"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/swrap/sriovnet"
	"github.com/vishvananda/netlink"
	vishNetns "github.com/vishvananda/netns"

	//	errors2 "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const defaultCNIDir = "/var/lib/cni/sriov"
const maxSharedVf = 2

var logFile *os.File

type dpdkConf struct {
	PCIaddr    string `json:"pci_addr"`
	Ifname     string `json:"ifname"`
	KDriver    string `json:"kernel_driver"`
	DPDKDriver string `json:"dpdk_driver"`
	DPDKtool   string `json:"dpdk_tool"`
	VFID       int    `json:"vfid"`
}

type NetConf struct {
	types.NetConf
	DPDKMode     bool
	Sharedvf     bool
	DPDKConf     dpdkConf `json:"dpdk,omitempty"`
	CNIDir       string   `json:"cniDir"`
	IF0          string   `json:"if0"`
	IF0NAME      string   `json:"if0name"`
	L2Mode       bool     `json:"l2enable"`
	Vlan         int      `json:"vlan"`
	PfNetdevices []string `json:"pfNetdevices"`
}

type pfInfo struct {
	PFNdevName string
	NumVfs     int
}

type pfList []*pfInfo

func (s pfList) Len() int {
	return len(s)
}

func (s pfList) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s pfList) Less(i, j int) bool {
	return s[i].NumVfs > s[j].NumVfs
}

// Link names given as os.FileInfo need to be sorted by their Index

type LinksByIndex []os.FileInfo

// LinksByIndex implements sort.Inteface
func (l LinksByIndex) Len() int { return len(l) }

func (l LinksByIndex) Swap(i, j int) { l[i], l[j] = l[j], l[i] }

func (l LinksByIndex) Less(i, j int) bool {
	link_a, _ := netlink.LinkByName(l[i].Name())
	link_b, _ := netlink.LinkByName(l[j].Name())

	return link_a.Attrs().Index < link_b.Attrs().Index
}

func init() {
	//	logFile.Write([]byte("ENTERING init\n"))
	// this ensures that main runs only on main thread (thread group leader).
	// since namespace ops (unshare, setns) are done for a single thread, we
	// must ensure that the goroutine does not jump from OS thread to thread
	runtime.LockOSThread()
}

func setVfBandwidthLimits(pfName string, vfNumber string, minTxRate string, maxTxRate string) error {
        cmnd := exec.Command("/sbin/ip", "link", "set", "dev", pfName, "vf", vfNumber, "max_tx_rate", maxTxRate, "min_tx_rate", minTxRate)
        err := cmnd.Start()
        if(err != nil) {
		return fmt.Errorf("Iproute2 command failed with message: %s", err)
        }

	return nil
}

func checkIf0name(ifname string) bool {
	if logFile != nil {
		logFile.Write([]byte("ENTERING checkIf0name\n"))
	}
	op := []string{"eth0", "eth1", "lo", ""}
	for _, if0name := range op {
		if strings.Compare(if0name, ifname) == 0 {
			return false
		}
	}

	return true
}

func loadConf(bytes []byte) (*NetConf, error) {
	if logFile != nil {
		logFile.Write([]byte("ENTERING loadConf\n"))
	}
	n := &NetConf{}
	if err := json.Unmarshal(bytes, n); err != nil {
		return nil, fmt.Errorf("failed to load netconf: %v", err)
	}

	if n.IF0NAME != "" {
		err := checkIf0name(n.IF0NAME)
		if err != true {
			return nil, fmt.Errorf(`"if0name" field should not be  equal to (eth0 | eth1 | lo | ""). It specifies the virtualized interface name in the pod`)
		}
	}

	if n.IF0 == "" && len(n.PfNetdevices) == 0 {
		return nil, fmt.Errorf(`"if0" or "pfNetdevices" field is required. It specifies the host interface name to virtualize`)
	}

	if n.CNIDir == "" {
		n.CNIDir = defaultCNIDir
	}

	if (dpdkConf{}) != n.DPDKConf {
		n.DPDKMode = true
	}

	return n, nil
}

func saveScratchNetConf(containerID, dataDir string, netconf []byte) error {
	if logFile != nil {
		logFile.Write([]byte("ENTERING saveScratchNetConf\n"))
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return fmt.Errorf("failed to create the sriov data directory(%q): %v", dataDir, err)
	}

	path := filepath.Join(dataDir, containerID)

	err := ioutil.WriteFile(path, netconf, 0600)
	if err != nil {
		return fmt.Errorf("failed to write container data in the path(%q): %v", path, err)
	}

	return err
}

func consumeScratchNetConf(containerID, dataDir string) ([]byte, error) {
	if logFile != nil {
		logFile.Write([]byte("ENTERING consumeScratchNetConf\n"))
	}
	path := filepath.Join(dataDir, containerID)
	defer os.Remove(path)

	data, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read container data in the path(%q): %v", path, err)
	}

	return data, err
}

func saveNetConf(cid, dataDir string, conf *NetConf) error {
	if logFile != nil {
		logFile.Write([]byte("ENTERING saveNetConf\n"))
	}
	confBytes, err := json.Marshal(conf)
	if err != nil {
		return fmt.Errorf("error serializing delegate netconf: %v", err)
	}

	s := []string{cid, conf.DPDKConf.Ifname}
	cRef := strings.Join(s, "-")

	// save the rendered netconf for cmdDel
	if err = saveScratchNetConf(cRef, dataDir, confBytes); err != nil {
		return err
	}

	return nil
}

func (nc *NetConf) getNetConf(cid, podIfName, dataDir string, conf *NetConf) error {
	if logFile != nil {
		logFile.Write([]byte("ENTERING getNetConf\n"))
	}
	s := []string{cid, podIfName}
	cRef := strings.Join(s, "-")

	confBytes, err := consumeScratchNetConf(cRef, dataDir)
	if err != nil {
		return err
	}

	if err = json.Unmarshal(confBytes, nc); err != nil {
		return fmt.Errorf("failed to parse netconf: %v", err)
	}

	return nil
}

// Devices is ordered by its number of VFs, device that has the
// most number of vfs will be first in the list.
func getOrderedPF(devices []string) ([]string, error) {
	if logFile != nil {
		logFile.Write([]byte("ENTERING getOrderedPF\n"))
	}
	//check pf devices
	var pfs pfList
	for _, pfName := range devices {
		vfDir := fmt.Sprintf("/sys/class/net/%s/device/virtfn*/net/*", pfName) /* */
		vfs, err := filepath.Glob(vfDir)
		if err != nil {
			return nil, err
		}
		pfs = append(pfs, &pfInfo{pfName, len(vfs)})
	}

	sort.Sort(pfs)

	var result []string
	for _, pf := range pfs {
		result = append(result, pf.PFNdevName)
	}
	return result, nil
}

func enabledpdkmode(conf *dpdkConf, ifname string, dpdkmode bool) error {
	if logFile != nil {
		logFile.Write([]byte("ENTERING enabledpdkmode\n"))
	}
	stdout := &bytes.Buffer{}
	var driver string
	var device string

	if dpdkmode != false {
		driver = conf.DPDKDriver
		device = ifname
	} else {
		driver = conf.KDriver
		device = conf.PCIaddr
	}

	cmd := exec.Command(conf.DPDKtool, "-b", driver, device)
	cmd.Stdout = stdout
	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("DPDK binding failed with err msg %q:", stdout.String())
	}

	stdout.Reset()
	return nil
}

func getpciaddress(ifName string, vf int) (string, error) {
	if logFile != nil {
		logFile.Write([]byte("ENTERING getpciaddress\n"))
	}
	var pciaddr string
	vfDir := fmt.Sprintf("/sys/class/net/%s/device/virtfn%d", ifName, vf)
	dirInfo, err := os.Lstat(vfDir)
	if err != nil {
		return pciaddr, fmt.Errorf("can't get the symbolic link of virtfn%d dir of the device %q: %v", vf, ifName, err)
	}

	if (dirInfo.Mode() & os.ModeSymlink) == 0 {
		return pciaddr, fmt.Errorf("No symbolic link for the virtfn%d dir of the device %q", vf, ifName)
	}

	pciinfo, err := os.Readlink(vfDir)
	if err != nil {
		return pciaddr, fmt.Errorf("can't read the symbolic link of virtfn%d dir of the device %q: %v", vf, ifName, err)
	}

	pciaddr = pciinfo[len("../"):]
	return pciaddr, nil
}

func getSharedPF(ifName string) (string, error) {
	if logFile != nil {
		logFile.Write([]byte("ENTERING getSharedPF\n"))
	}
	pfName := ""
	pfDir := fmt.Sprintf("/sys/class/net/%s", ifName)
	dirInfo, err := os.Lstat(pfDir)
	if err != nil {
		return pfName, fmt.Errorf("can't get the symbolic link of the device %q: %v", ifName, err)
	}

	if (dirInfo.Mode() & os.ModeSymlink) == 0 {
		return pfName, fmt.Errorf("No symbolic link for dir of the device %q", ifName)
	}

	fullpath, err := filepath.EvalSymlinks(pfDir)
	parentDir := fullpath[:len(fullpath)-len(ifName)]
	dirList, err := ioutil.ReadDir(parentDir)

	for _, file := range dirList {
		if file.Name() != ifName {
			pfName = file.Name()
			return pfName, nil
		}
	}

	return pfName, fmt.Errorf("Shared PF not found")
}

func getsriovNumfs(ifName string) (int, error) {
	if logFile != nil {
		logFile.Write([]byte("ENTERING getsriovNumfs\n"))
	}
	var vfTotal int

	sriovFile := fmt.Sprintf("/sys/class/net/%s/device/sriov_numvfs", ifName)
	if _, err := os.Lstat(sriovFile); err != nil {
		return vfTotal, fmt.Errorf("failed to open the sriov_numfs of device %q: %v", ifName, err)
	}

	data, err := ioutil.ReadFile(sriovFile)
	if err != nil {
		return vfTotal, fmt.Errorf("failed to read the sriov_numfs of device %q: %v", ifName, err)
	}

	if len(data) == 0 {
		return vfTotal, fmt.Errorf("no data in the file %q", sriovFile)
	}

	sriovNumfs := strings.TrimSpace(string(data))
	i64, err := strconv.ParseInt(sriovNumfs, 10, 0)
	if err != nil {
		return vfTotal, fmt.Errorf("failed to convert sriov_numfs(byte value) to int of device %q: %v", ifName, err)
	}

	return int(i64), nil
}

func setSharedVfVlan(ifName string, vfIdx int, vlan int) error {
	if logFile != nil {
		logFile.Write([]byte("ENTERING setSharedVfVlan\n"))
	}
	var err error
	var sharedifName string

	vfDir := fmt.Sprintf("/sys/class/net/%s/device/net", ifName)
	if _, err := os.Lstat(vfDir); err != nil {
		return fmt.Errorf("failed to open the net dir of the device %q: %v", ifName, err)
	}

	infos, err := ioutil.ReadDir(vfDir)
	if err != nil {
		return fmt.Errorf("failed to read the net dir of the device %q: %v", ifName, err)
	}

	if len(infos) != maxSharedVf {
		return fmt.Errorf("Given PF - %q is not having shared VF", ifName)
	}

	for _, dir := range infos {
		if strings.Compare(ifName, dir.Name()) != 0 {
			sharedifName = dir.Name()
		}
	}

	if sharedifName == "" {
		return fmt.Errorf("Shared ifname can't be empty")
	}

	iflink, err := netlink.LinkByName(sharedifName)
	if err != nil {
		return fmt.Errorf("failed to lookup the shared ifname %q: %v", sharedifName, err)
	}

	if err := netlink.LinkSetVfVlan(iflink, vfIdx, vlan); err != nil {
		return fmt.Errorf("failed to set vf %d vlan: %v for shared ifname %q", vfIdx, err, sharedifName)
	}

	return nil
}

func configSriov(master string) error {
	if logFile != nil {
		logFile.Write([]byte("ENTERING configSriov\n"))
	}
	err := sriovnet.EnableSriov(master)
	if err != nil {
		return err
	}

	handle, err2 := sriovnet.GetPfNetdevHandle(master)
	if err2 != nil {
		return err2
	}
	// configure privilege VFs
	err2 = sriovnet.ConfigVfs(handle, true)
	if err2 != nil {
		return err2
	}
	return nil
}

func setupVF(conf *NetConf, ifName string, podifName string, cid string, netns ns.NetNS, pod_interfaces_required knapsack_pod_placement.RdmaInterfaceRequest) (*int, error) {
	if logFile != nil {
		logFile.Write([]byte("ENTERING setupVF\n"))
	}

	var vfIdx int
	var infos []os.FileInfo
	var pciAddr string

	m, err := netlink.LinkByName(ifName)
	if err != nil {
		return nil, fmt.Errorf("failed to lookup master %q: %v", ifName, err)
	}

	// get the ifname sriov vf num
	vfTotal, err := getsriovNumfs(ifName)
	if err != nil {
		return nil, err
	}

	for vf := 0; vf <= (vfTotal - 1); vf++ {
		vfDir := fmt.Sprintf("/sys/class/net/%s/device/virtfn%d/net", ifName, vf)
		if _, err := os.Lstat(vfDir); err != nil {
			if vf == (vfTotal - 1) {
				return nil, fmt.Errorf("failed to open the virtfn%d dir of the device %q: %v", vf, ifName, err)
			}
			continue
		}

		infos, err = ioutil.ReadDir(vfDir)
		if err != nil {
			return nil, fmt.Errorf("failed to read the virtfn%d dir of the device %q: %v", vf, ifName, err)
		}

		if (len(infos) == 0) && (vf == (vfTotal - 1)) {
			return nil, fmt.Errorf("no Virtual function exist in directory %s, last vf is virtfn%d", vfDir, vf)
		}

		if (len(infos) == 0) && (vf != (vfTotal - 1)) {
			continue
		}

		if len(infos) == maxSharedVf {
			conf.Sharedvf = true
		}

		if len(infos) <= maxSharedVf {
			vfIdx = vf
			pciAddr, err = getpciaddress(ifName, vfIdx)
			if err != nil {
				return nil, fmt.Errorf("err in getting pci address - %q", err)
			}

			if logFile != nil {
				logFile.Write([]byte("INFO: SETTING UP MINMAX RATE\n"))
			}

			err := setVfBandwidthLimits(
				ifName,
				fmt.Sprintf("%d", vfIdx),
				fmt.Sprintf("%d", pod_interfaces_required.MinTxRate),
				fmt.Sprintf("%d", pod_interfaces_required.MaxTxRate))
			if(err != nil) {
				return &vfIdx, fmt.Errorf("Failed setting the min and max tx rates on PF[%s] VF[%d]: %s", ifName, vf, err)
			}

			// if err = netlink.LinkSetMinMaxVfTxRate(m, vfIdx, uint32(pod_interfaces_required.MinTxRate), uint32(pod_interfaces_required.MaxTxRate)); err != nil {
			// 	return fmt.Errorf("error setting min/max rate on PF[%s] VF[%d]: %s", ifName, vf, err)
			// }
			if logFile != nil {
				str := fmt.Sprintf("INFO: Finished setting up min max rate: PF[%s] VF[%d] Rates[%v]\n", ifName, vf, pod_interfaces_required)
				logFile.Write([]byte(str))
			}
			break
		} else {
			return nil, fmt.Errorf("mutiple network devices in directory %s", vfDir)
		}
	}

	// VF NIC name
	if len(infos) != 1 && len(infos) != maxSharedVf {
		return &vfIdx, fmt.Errorf("no virutal network resources avaiable for the %q", ifName)
	}

	if conf.Sharedvf != false && conf.L2Mode != true {
		return &vfIdx, fmt.Errorf("l2enable mode must be true to use shared net interface %q", ifName)
	}

	if conf.Vlan != 0 {
		if err = netlink.LinkSetVfVlan(m, vfIdx, conf.Vlan); err != nil {
			return &vfIdx, fmt.Errorf("failed to set vf %d vlan: %v", vfIdx, err)
		}

		if conf.Sharedvf {
			if err = setSharedVfVlan(ifName, vfIdx, conf.Vlan); err != nil {
				return &vfIdx, fmt.Errorf("failed to set shared vf %d vlan: %v", vfIdx, err)
			}
		}
	}

	conf.DPDKConf.PCIaddr = pciAddr
	conf.DPDKConf.Ifname = podifName
	conf.DPDKConf.VFID = vfIdx
	if conf.DPDKMode != false {
		if err = saveNetConf(cid, conf.CNIDir, conf); err != nil {
			return &vfIdx, err
		}
		return &vfIdx, enabledpdkmode(&conf.DPDKConf, infos[0].Name(), true)
	}

	// Sort links name if there are 2 or more PF links found for a VF;
	if len(infos) > 1 {
		// sort Links FileInfo by their Link indices
		sort.Sort(LinksByIndex(infos))
	}

	var vfName string
	for i := 1; i <= len(infos); i++ {
		vfDev, err := netlink.LinkByName(infos[i-1].Name())
		if err != nil {
			return &vfIdx, fmt.Errorf("failed to lookup vf device %q: %v", infos[i-1].Name(), err)
		}
		// change name if it is eth0
		if infos[i-1].Name() == "eth0" {
			netlink.LinkSetDown(vfDev)
			vfName = fmt.Sprintf("sriov%v", rand.Intn(9999))
			renameLink(infos[i-1].Name(), vfName)
		} else {
			vfName = infos[i-1].Name()
		}
		if err = netlink.LinkSetUp(vfDev); err != nil {
			return &vfIdx, fmt.Errorf("failed to setup vf %d device: %v", vfIdx, err)
		}

		// move VF device to ns
		if err = netlink.LinkSetNsFd(vfDev, int(netns.Fd())); err != nil {
			return &vfIdx, fmt.Errorf("failed to move vf %d to netns: %v", vfIdx, err)
		}
	}

	return &vfIdx, netns.Do(func(_ ns.NetNS) error {

		ifName := podifName
		for i := 1; i <= len(infos); i++ {
			if len(infos) == maxSharedVf && i == len(infos) {
				ifName = podifName + fmt.Sprintf("d%d", i-1)
			}

			err := renameLink(vfName, ifName)
			if err != nil {
				return fmt.Errorf("failed to rename %d vf of the device %q to %q: %v", vfIdx, infos[i-1].Name(), ifName, err)
			}

			// for L2 mode enable the pod net interface
			if conf.L2Mode != false {
				err = setUpLink(ifName)
				if err != nil {
					return fmt.Errorf("failed to set up the pod interface name %q: %v", ifName, err)
				}
			}
		}
		if err = saveNetConf(cid, conf.CNIDir, conf); err != nil {
			return fmt.Errorf("failed to save pod interface name %q: %v", ifName, err)
		}
		return nil
	})
}

func releaseVF(conf *NetConf, podifName string, cid string, netns ns.NetNS, pfName string, vfNumber *int) error {
	if logFile != nil {
		logFile.Write([]byte("ENTERING releaseVF\n"))
	}
	log.Println("RIT-CNI: RELEASEVF")

	nf := &NetConf{}
	// get the net conf in cniDir
	if err := nf.getNetConf(cid, podifName, conf.CNIDir, conf); err != nil {
		return err
	}

	// check for the DPDK mode and release the allocated DPDK resources
	if nf.DPDKMode != false {
		// bind the sriov vf to the kernel driver
		if err := enabledpdkmode(&nf.DPDKConf, nf.DPDKConf.Ifname, false); err != nil {
			return fmt.Errorf("DPDK: failed to bind %s to kernel space: %s", nf.DPDKConf.Ifname, err)
		}

		// reset vlan for DPDK code here
		pfLink, err := netlink.LinkByName(conf.IF0)
		if err != nil {
			return fmt.Errorf("DPDK: master device %s not found: %v", conf.IF0, err)
		}

		if err = netlink.LinkSetVfVlan(pfLink, nf.DPDKConf.VFID, 0); err != nil {
			return fmt.Errorf("DPDK: failed to reset vlan tag for vf %d: %v", nf.DPDKConf.VFID, err)
		}

		return nil
	}

	initns, err := ns.GetCurrentNS()
	if err != nil {
		return fmt.Errorf("failed to get init netns: %v", err)
	}

	if err = netns.Set(); err != nil {
		return fmt.Errorf("failed to enter netns %q: %v", netns, err)
	}

	if conf.L2Mode != false {
		//check for the shared vf net interface
		ifName := podifName + "d1"
		_, err := netlink.LinkByName(ifName)
		if err == nil {
			conf.Sharedvf = true
		}

	}

	if err != nil {
		fmt.Errorf("Enable to get shared PF device: %v", err)
	}

	for i := 1; i <= maxSharedVf; i++ {
		ifName := podifName
		pfName := nf.IF0
		if i == maxSharedVf {
			ifName = podifName + fmt.Sprintf("d%d", i-1)
			pfName, err = getSharedPF(nf.IF0)
			if err != nil {
				return fmt.Errorf("Failed to look up shared PF device: %v:", err)
			}
		}

		// get VF device
		vfDev, err := netlink.LinkByName(ifName)
		if err != nil {
			return fmt.Errorf("failed to lookup vf device %q: %v", ifName, err)
		}

		// device name in init netns
		index := vfDev.Attrs().Index
		devName := fmt.Sprintf("dev%d", index)

		// shutdown VF device
		if err = netlink.LinkSetDown(vfDev); err != nil {
			return fmt.Errorf("failed to down vf device %q: %v", ifName, err)
		}

		log.Printf("RIT-CNI: rename link: %s to %s\n", ifName, devName)
		// rename VF device
		err = renameLink(ifName, devName)
		if err != nil {
			return fmt.Errorf("failed to rename vf device %q to %q: %v", ifName, devName, err)
		}

		// move VF device to init netns
		if err = netlink.LinkSetNsFd(vfDev, int(initns.Fd())); err != nil {
			return fmt.Errorf("failed to move vf device %q to init netns: %v", ifName, err)
		}

		// reset vlan
		if conf.Vlan != 0 {
			err = initns.Do(func(_ ns.NetNS) error {
				return resetVfVlan(pfName, devName)
			})
			if err != nil {
				return fmt.Errorf("failed to reset vlan: %v", err)
			}
		}

		if vfNumber != nil {
			err = initns.Do(func(_ ns.NetNS) error {
				log.Println("RIT-CNI: doing initns stuff: ", *vfNumber)
				if err = setVfBandwidthLimits(pfName, fmt.Sprintf("%d", *vfNumber), "0", "0"); err != nil {
					return fmt.Errorf("Failed resetting bandwidth limits: %s", err)
				}
				// if err = netlink.LinkSetMinMaxVfTxRate(vfDev, int(foundVf.VFNumber), uint32(0), uint32(0)); err != nil {
				// 	log.Printf("Error setting mac address back to 0: %s\n", podInterface.HardwareAddr.String())
				// 	return fmt.Errorf("Error setting mac address back to 0: %s\n", podInterface.HardwareAddr.String())
				// }
				return nil
			})
			if err != nil {
				log.Println("RIT-CNI: ERROR setting the stuff")
				return fmt.Errorf("failed to reset min/max speed: %v", err)
			}
		}

		//break the loop, if the namespace has no shared vf net interface
		if conf.Sharedvf != true {
			break
		}
	}

	return nil
}

func releaseVFCustom(conf *NetConf, podInterface net.Interface, cid string, podNetNs string, pfs []rdma_hardware_info.PF) error {
	log.Println("RIT-CNI: RELEASEVF")
	// secure the thread for namespace operations
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	netns, err := ns.GetNS(podNetNs)
	if err != nil {
		return fmt.Errorf("failed to open pod netnamespace %q: %v", netns, err)
	}
	defer netns.Close()

	log.Println("RIT-CNI: starting up th netConf")
	nf := &NetConf{}
	// get the net conf in cniDir
	if err := nf.getNetConf(cid, podInterface.Name, conf.CNIDir, conf); err != nil {
		return err
	}

	// check for the DPDK mode and release the allocated DPDK resources
	if nf.DPDKMode != false {
		// bind the sriov vf to the kernel driver
		if err := enabledpdkmode(&nf.DPDKConf, nf.DPDKConf.Ifname, false); err != nil {
			return fmt.Errorf("DPDK: failed to bind %s to kernel space: %s", nf.DPDKConf.Ifname, err)
		}

		// reset vlan for DPDK code here
		pfLink, err := netlink.LinkByName(conf.IF0)
		if err != nil {
			return fmt.Errorf("DPDK: master device %s not found: %v", conf.IF0, err)
		}

		if err = netlink.LinkSetVfVlan(pfLink, nf.DPDKConf.VFID, 0); err != nil {
			return fmt.Errorf("DPDK: failed to reset vlan tag for vf %d: %v", nf.DPDKConf.VFID, err)
		}

		return nil
	}

	log.Println("RIT-CNI: current ns")
	initns, err := ns.GetCurrentNS()
	if err != nil {
		return fmt.Errorf("failed to get init the current netns: %v", err)
	}
	defer initns.Close()

	log.Println("RIT-CNI: net ns set it up ")
	if err = netns.Set(); err != nil {
		return fmt.Errorf("failed to enter pod netns %q: %v", netns, err)
	}
	defer netns.Close()
	defer func() {
		log.Println("RIT-CNI: closing initns")
		if err = initns.Set(); err != nil {
			log.Printf("Error cleaning up: failed to setting back namespace %q: %v\n", initns, err)
		} else {
			log.Println("RIT-CNI: success!")
		}
	}()

	if conf.L2Mode != false {
		//check for the shared vf net interface
		ifName := podInterface.Name + "d1"
		_, err := netlink.LinkByName(ifName)
		if err == nil {
			conf.Sharedvf = true
		}

	}

	if err != nil {
		fmt.Errorf("Enable to get shared PF device: %v", err)
	}

	for i := 1; i <= maxSharedVf; i++ {

		log.Println("RIT-CNI: yasdfasdf ", podInterface.Name)
		ifName := podInterface.Name
		pfName := nf.IF0
		if i == maxSharedVf {
			ifName = podInterface.Name + fmt.Sprintf("d%d", i-1)
			pfName, err = getSharedPF(nf.IF0)
			if err != nil {
				return fmt.Errorf("Failed to look up shared PF device: %v:", err)
			}
		}

		// get VF device
		vfDev, err := netlink.LinkByName(ifName)
		if err != nil {
			return fmt.Errorf("failed to lookup vf device %q: %v", ifName, err)
		}

		// device name in init netns
		index := vfDev.Attrs().Index
		devName := fmt.Sprintf("dev%d", index)

		// shutdown VF device
		if err = netlink.LinkSetDown(vfDev); err != nil {
			return fmt.Errorf("failed to down vf device %q: %v", ifName, err)
		}

		log.Printf("RIT-CNI: rename link: %s to %s\n", ifName, devName)
		// rename VF device
		err = renameLink(ifName, devName)
		if err != nil {
			return fmt.Errorf("failed to rename vf device %q to %q: %v", ifName, devName, err)
		}

		// move VF device to init netns
		if err = netlink.LinkSetNsFd(vfDev, int(initns.Fd())); err != nil {
			return fmt.Errorf("failed to move vf device %q to init netns: %v", ifName, err)
		}

		log.Println("RIT-CNI: vlan")
		// reset vlan
		if conf.Vlan != 0 {
			err = initns.Do(func(_ ns.NetNS) error {
				return resetVfVlan(pfName, devName)
			})
			if err != nil {
				return fmt.Errorf("failed to reset vlan: %v", err)
			}
		}

		var foundVf *rdma_hardware_info.VF
		var foundPfName string
		for _, pf := range pfs {
			foundVf = pf.FindAssociatedMac(podInterface.HardwareAddr.String())
			if foundVf != nil {
				foundPfName = pf.Name
				break
			}
		}
		if foundVf == nil {
			log.Printf("Error mac address never found: %s\n", podInterface.HardwareAddr.String())
		} else {
			err = initns.Do(func(_ ns.NetNS) error {
				log.Println("RIT-CNI: doing initns stuff ", foundVf.VFNumber, vfDev, foundVf)
				if err = setVfBandwidthLimits(foundPfName, fmt.Sprintf("%d", foundVf.VFNumber), "0", "0"); err != nil {
					return fmt.Errorf("Failed resetting bandwidth limits: %s", err)
				}
				// if err = netlink.LinkSetMinMaxVfTxRate(vfDev, int(foundVf.VFNumber), uint32(0), uint32(0)); err != nil {
				// 	log.Printf("Error setting mac address back to 0: %s\n", podInterface.HardwareAddr.String())
				// 	return fmt.Errorf("Error setting mac address back to 0: %s\n", podInterface.HardwareAddr.String())
				// }
				return nil
			})
			if err != nil {
				log.Println("RIT-CNI: ERROR setting the stuff")
				return fmt.Errorf("failed to reset min/max speed: %v", err)
			}
		}

		//break the loop, if the namespace has no shared vf net interface
		if conf.Sharedvf != true {
			break
		}
	}

	log.Println("RIT-CNI: done")

	return nil
}

func resetVfVlan(pfName, vfName string) error {
	if logFile != nil {
		logFile.Write([]byte("ENTERING resetVfVlan\n"))
	}

	// get the ifname sriov vf num
	vfTotal, err := getsriovNumfs(pfName)
	if err != nil {
		return err
	}

	if vfTotal <= 0 {
		return fmt.Errorf("no virtual function in the device %q", pfName)
	}

	// Get VF id
	var vf int
	idFound := false
	for vf = 0; vf < vfTotal; vf++ {
		vfDir := fmt.Sprintf("/sys/class/net/%s/device/virtfn%d/net/%s", pfName, vf, vfName)
		if _, err := os.Stat(vfDir); !os.IsNotExist(err) {
			idFound = true
			break
		}
	}

	if !idFound {
		return fmt.Errorf("failed to get VF id for %s", vfName)
	}

	pfLink, err := netlink.LinkByName(pfName)
	if err != nil {
		return fmt.Errorf("Master device %s not found\n", pfName)
	}

	if err = netlink.LinkSetVfVlan(pfLink, vf, 0); err != nil {
		return fmt.Errorf("failed to reset vlan tag for vf %d: %v", vf, err)
	}
	return nil
}

func validateSriovEnabled(pfName string) error {
	if logFile != nil {
		logFile.Write([]byte("ENTERING validateSriovEnabled\n"))
	}
	vfTotal, err := getsriovNumfs(pfName)
	if err != nil {
		return err
	}

	if vfTotal == 0 {
		err = configSriov(pfName)
		if err != nil {
			return fmt.Errorf("no virtual function in the device %q", pfName)
		}
	}
	return nil
}

func getPFs(if0 string, devs []string) ([]string, error) {
	if logFile != nil {
		logFile.Write([]byte("ENTERING getPFs\n"))
	}
	pfs := []string{}
	if if0 != "" {
		pfs = append(pfs, if0)
	}

	for _, pf := range devs {
		if pf != if0 {
			pfs = append(pfs, pf)
		}
	}

	for _, pf := range pfs {
		err := validateSriovEnabled(pf)
		if err != nil {
			return nil, err
		}
	}

	orderedPF, err := getOrderedPF(pfs)
	if err != nil {
		return nil, err
	}
	return orderedPF, nil
}

func getContainer(namespace, containerID string) {
	if logFile != nil {
		logFile.Write([]byte("ENTERING getContainer\n"))
	}
	config, err := clientcmd.BuildConfigFromFlags("", "/etc/kubernetes/kubelet.conf")
	if err != nil {
		logFile.Write([]byte("An error occured when reading config file.\n"))
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		logFile.Write([]byte("An error occured when reading config file.\n"))
	}
	pods, err := clientset.CoreV1().Pods(namespace).List(metav1.ListOptions{})
	if err != nil {
		logFile.Write([]byte("An error occured when reading config file.\n"))
	}
	logFile.Write([]byte(fmt.Sprintf("There are %d pods in the cluster\n", len(pods.Items))))
}

func cmdAdd(args *skel.CmdArgs) error {
	if logFile != nil {
		logFile.Write([]byte("ENTERING cmdAdd\n"))
	}
	logFile.Write([]byte(fmt.Sprintf("%+v", args)))

	pod_name := ""
	pod_ns := ""
	for _, arg_mapping := range strings.Split((*args).Args, ";") {
		key_value_pair := strings.Split(arg_mapping, "=")
		if len(key_value_pair) == 2 {
			if key_value_pair[0] == "K8S_POD_NAMESPACE" {
				pod_ns = key_value_pair[1]
			} else if key_value_pair[0] == "K8S_POD_NAME" {
				pod_name = key_value_pair[1]
			}
		}
	}
	//	if(logFile != nil) {logFile.Write([]byte("CONTAINER ID: "))}
	//	if(logFile != nil) {logFile.Write([]byte(args.ContainerID))}
	//	if(logFile != nil) {logFile.Write([]byte("\nNetork Namespace: "))}
	//	if(logFile != nil) {logFile.Write([]byte(args.Netns))}
	if logFile != nil {
		logFile.Write([]byte("\n"))
	}

	pod_interfaces_required := getPodRequirements(pod_name, pod_ns)
	pfs_available, err := rdma_hardware_info.QueryNode("127.0.0.1", rdma_hardware_info.DefaultPort, 1500)
	if err != nil {
		log.Fatal("Could not determine what RDMA hardware resources are available.")
	}

	pod_interface_placements, placement_successful := knapsack_pod_placement.PlacePod(pod_interfaces_required, pfs_available, false)
	if !placement_successful {
		log.Fatal("Unable to fit pod into available RDMA resources on node.")
	}

	n, err := loadConf(args.StdinData)
	if err != nil {
		return fmt.Errorf("failed to load netconf: %v", err)
	}

	container_id_log, _ := os.OpenFile(fmt.Sprintf("/opt/cni/bin/%s", args.ContainerID), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	container_id_log.Write([]byte("aaaaa\n"))
	container_id_log.Close()

	if logFile != nil {
		logFile.Write(args.StdinData)
	}
	if logFile != nil {
		logFile.Write([]byte("\n"))
	}

	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %v", netns, err)
	}
	defer netns.Close()

	old_ifname := os.Getenv("CNI_IFNAME")
	defer os.Setenv("CNI_IFNAME", old_ifname)
	if n.IF0NAME != "" {
		args.IfName = n.IF0NAME
		os.Setenv("CNI_IFNAME", args.IfName)
	}

	for iPodPlacement, podPlacement := range pod_interface_placements {
		pf := pfs_available[podPlacement]

		ifName := fmt.Sprintf("eth%d", iPodPlacement)
		var vfNum *int
		vfNum, err = setupVF(n, pf.Name, ifName, args.ContainerID, netns, pod_interfaces_required[iPodPlacement])
		//defer func is called when errors are encountered, will rollback any changes made
		defer func(internalIfName string) {
			if err != nil {
				err = netns.Do(func(_ ns.NetNS) error {
					_, err := netlink.LinkByName(internalIfName)
					return err
				})
				if err == nil {
					releaseVF(n, internalIfName, args.ContainerID, netns, pf.Name, vfNum)
				}
			}
		}(ifName)
		if err != nil {
			return fmt.Errorf("failed to set up pod interface %q from the device %s: %v", ifName, pf.Name, err)
		}
	}

	// skip the IPAM allocation for the DPDK and L2 mode
	var result *types.Result
	if n.DPDKMode != false || n.L2Mode != false {
		return result.Print()
	}

	// run the IPAM plugin and get back the config to apply
	result, err = ipam.ExecAdd(n.IPAM.Type, args.StdinData)
	if err != nil {
		return fmt.Errorf("failed to set up IPAM plugin type %q from the device %q: %v", n.IPAM.Type, n.IF0, err)
	}
	if result.IP4 == nil {
		return errors.New("IPAM plugin returned missing IPv4 config")
	}
	defer func() {
		if err != nil {
			ipam.ExecDel(n.IPAM.Type, args.StdinData)
		}
	}()
	err = netns.Do(func(_ ns.NetNS) error {
		return ipam.ConfigureIface(args.IfName, result)
	})
	if err != nil {
		return err
	}

	result.DNS = n.DNS
	return result.Print()
}

func getNamespaceInterfaces(netnsName string) ([]net.Interface, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// get the current namespace
	origns, err := vishNetns.Get()
	if err != nil {
		return nil, fmt.Errorf("error getting origin namespace: %s", err)
	}
	defer origns.Close()

	// get the names from the path
	newns, err := vishNetns.GetFromPath(netnsName)
	if err != nil {
		return nil, fmt.Errorf("error getting netns namespace: %s", err)
	}
	defer newns.Close()

	err = vishNetns.Set(newns)
	if err != nil {
		return nil, fmt.Errorf("error setting to ns namespace: %s", err)
	}

	// get all interfaces
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("error getting network interfaces: %s", err)
	}

	// set back to original interfaces
	err = vishNetns.Set(origns)
	if err != nil {
		return nil, fmt.Errorf("error setting back to origin namespace: %s", err)
	}
	return ifaces, nil
}

func cmdDel(args *skel.CmdArgs) error {
	//	logFile, _ = os.OpenFile("/opt/cni/bin/asdf", os.O_APPEND | os.O_CREATE | os.O_WRONLY, 0644)
	//	defer logFile.Close()
	if logFile != nil {
		logFile.Write([]byte("ENTERING cmdDel\n"))
	}
	n, err := loadConf(args.StdinData)
	if err != nil {
		return err
	}

	log.Println("RIT-CNI: cmdDel")

	// skip the IPAM release for the DPDK and L2 mode
	if n.IPAM.Type != "" {
		err = ipam.ExecDel(n.IPAM.Type, args.StdinData)
		if err != nil {
			return err
		}
	}

	if args.Netns == "" {
		return nil
	}

	// netns, err := ns.GetNS(args.Netns)
	// if err != nil {
	// 	return fmt.Errorf("failed to open netns %q: %v", netns, err)
	// }
	// defer netns.Close()

	// old_ifname := os.Getenv("CNI_IFNAME")
	// defer os.Setenv("CNI_IFNAME", old_ifname)
	// if n.IF0NAME != "" {
	// 	args.IfName = n.IF0NAME
	// 	os.Setenv("CNI_IFNAME", args.IfName)
	// }

	log.Println("RIT-CNI: PREPARING NAMESPACE 6")
	log.Println("RIT-CNI: netns:", args.Netns)
	log.Printf("RIT-CNI: ARGS: %+v\n", args)
	interfaces, err := getNamespaceInterfaces(args.Netns)
	if err != nil {
		return fmt.Errorf("Error getting iterfaces: %s", err)
	}

	pfs_available, err := rdma_hardware_info.QueryNode("127.0.0.1", rdma_hardware_info.DefaultPort, 1500)
	if err != nil {
		log.Println("Error: could not determine what RDMA hardware resources are available")
	}

	for _, netIntf := range interfaces {
		log.Printf("RIT-CNI: Going through ifname: %s\n", netIntf.Name)
		if strings.HasPrefix(netIntf.Name, "eth") {
			_, err := strconv.Atoi(netIntf.Name[3:])
			if err != nil {
				continue
			}
			if err = releaseVFCustom(n, netIntf, args.ContainerID, args.Netns, pfs_available); err != nil {
				log.Printf("Error releasing vf %+v: %s", netIntf, err)
				continue
			}
		}
	}
	log.Println("RIT-CNI: CMDDONE")

	// //get host namespace
	// initns, err := ns.GetCurrentNS()
	// if err != nil {
	// 	return fmt.Errorf("cmdDel, failed to get init netns: %v", err)
	// }

	// //change pods namespace
	// if err = netns.Set(); err != nil {
	// 	return fmt.Errorf("cmdDel, failed to enter netns %q: %v", netns, err)
	// }

	// netdevDir := fmt.Sprintf("/sys/class/net/")
	// _, err = os.Stat(netdevDir)
	// if !os.IsNotExist(err) {
	// 	log.Println("RIT-CNI: found file directory")
	// 	// if it does exist
	// 	dirs, err := ioutil.ReadDir(netdevDir)
	// 	if err != nil {
	// 		return fmt.Errorf("failed to read devices dir")
	// 	}

	// 	for _, file := range dirs {
	// 		log.Printf("RIT-CNI: found ethernet: %s\n", file.Name())
	// 		if strings.HasPrefix(file.Name(), "eth") {
	// 			_, err := strconv.Atoi(file.Name()[3:])
	// 			if err != nil {
	// 				continue
	// 			}
	// 			netDevNames = append(netDevNames, file.Name())
	// 		}
	// 	}
	// 	log.Println("RIT-CNI: finished checking dirs")
	// }
	// log.Println("RIT-CNI: finished checking directory")
	// log.Println("RIT-CNI: ", netDevNames)
	// return nil
	// // })

	// log.Println("RIT-CNI: finished checking directory")
	// log.Println("RIT-CNI: ", netDevNames)

	// // //change pods namespace back to host namespace
	// // if err = initns.Set(); err != nil {
	// // 	return fmt.Errorf("cmdDel, failed to enter host namespace again %q: %v", initns, err)
	// // }

	// for _, ifName := range netDevNames {
	// 	log.Printf("RIT-CNI: Going through ifname: %s\n", ifName)
	// 	// if err = releaseVF(n, ifName, args.ContainerID, netns); err != nil {
	// 	// 	return err
	// 	// }

	// 	// //change pods namespace back to host namespace
	// 	// if err = initns.Set(); err != nil {
	// 	// 	return fmt.Errorf("cmdDel, failed to enter host namespace again in loop for interface[%s] %q: %v", ifName, initns, err)
	// 	// }
	// }
	// log.Println("RIT-CNI: CMDDONE")

	return nil
}

func renameLink(curName, newName string) error {
	link, err := netlink.LinkByName(curName)
	if err != nil {
		return fmt.Errorf("failed to lookup device %q: %v", curName, err)
	}

	return netlink.LinkSetName(link, newName)
}

func setUpLink(ifName string) error {
	link, err := netlink.LinkByName(ifName)
	if err != nil {
		return fmt.Errorf("failed to set up device %q: %v", ifName, err)
	}

	return netlink.LinkSetUp(link)
}

func getPodRequirements(pod_name string, pod_namespace string) []knapsack_pod_placement.RdmaInterfaceRequest {
	config, err := clientcmd.BuildConfigFromFlags("", "/etc/kubernetes/kubelet.conf")
	if err != nil {
		log.Fatal("RDMA CNI: Error building Kubernetes configuration from file /etc/kubernetes/kubelet.conf")
		//		logFile.Write([]byte("An error occured when reading config file.\n"))
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal("RDMA CNI: Error building clientset from Kubernetes config file.")
		//		logFile.Write([]byte("An error occured when reading config file.\n"))
	}

	pod, err := clientset.CoreV1().Pods(pod_namespace).Get(pod_name, metav1.GetOptions{})
	if err != nil {
		log.Fatal("RDMA CNI: Error retrieving pod information from Kubernetes API server.")
		//		logFile.Write([]byte("An error occured when reading config file.\n"))
	}

	//if no annotation about required RDMA interfaces was present
	if pod.ObjectMeta.Annotations["rdma_interfaces_required"] == "" {
		//the pod does not need any RDMA interfaces
		return []knapsack_pod_placement.RdmaInterfaceRequest{}
	}

	var interfaces_needed []knapsack_pod_placement.RdmaInterfaceRequest
	err = json.Unmarshal([]byte(pod.ObjectMeta.Annotations["rdma_interfaces_required"]), &interfaces_needed)
	if err != nil {
		log.Fatal("RDMA CNI: Error unmarshalling JSON for RDMA interface requirements.")
	}

	return interfaces_needed
}

func main() {
	logFile, _ = os.OpenFile("/opt/cni/bin/asdf", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	logFile.Write([]byte("ENTERING main\n"))

	/*
		config, err := clientcmd.BuildConfigFromFlags("", "/etc/kubernetes/kubelet.conf")
		if err != nil {
			logFile.Write([]byte("An error occured when reading config file.\n"))
		}

		clientset, err := kubernetes.NewForConfig(config)
		if err != nil {
			logFile.Write([]byte("An error occured when reading config file.\n"))
		}

		pods, err := clientset.CoreV1().Pods("").List(metav1.ListOptions{})
		if err != nil {
			logFile.Write([]byte("An error occured when reading config file.\n"))
		}

		for n := 0; n < len(pods.Items); n++ {
			logFile.Write([]byte(fmt.Sprintf("%+v", pods.Items[n].ObjectMeta)))
			logFile.Write([]byte("\n"))
		}
		logFile.Write([]byte(fmt.Sprintf("There are %d pods in the cluster\n", len(pods.Items))))
	*/

	skel.PluginMain(cmdAdd, cmdDel)
	logFile.Write([]byte("LEAVING main\n"))
	logFile.Close()
}
