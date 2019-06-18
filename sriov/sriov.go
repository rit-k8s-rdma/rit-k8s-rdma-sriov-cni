package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/Mellanox/sriovnet"
	"github.com/containernetworking/cni/pkg/ipam"
	"github.com/containernetworking/cni/pkg/ns"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/vishvananda/netlink"

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
		vfDir := fmt.Sprintf("/sys/class/net/%s/device/virtfn*/net/*", pfName)
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

func setupVF(conf *NetConf, ifName string, podifName string, cid string, netns ns.NetNS) error {
	if logFile != nil {
		logFile.Write([]byte("ENTERING setupVF\n"))
	}

	var vfIdx int
	var infos []os.FileInfo
	var pciAddr string

	m, err := netlink.LinkByName(ifName)
	if err != nil {
		return fmt.Errorf("failed to lookup master %q: %v", ifName, err)
	}

	// get the ifname sriov vf num
	vfTotal, err := getsriovNumfs(ifName)
	if err != nil {
		return err
	}

	for vf := 0; vf <= (vfTotal - 1); vf++ {
		vfDir := fmt.Sprintf("/sys/class/net/%s/device/virtfn%d/net", ifName, vf)
		if _, err := os.Lstat(vfDir); err != nil {
			if vf == (vfTotal - 1) {
				return fmt.Errorf("failed to open the virtfn%d dir of the device %q: %v", vf, ifName, err)
			}
			continue
		}

		infos, err = ioutil.ReadDir(vfDir)
		if err != nil {
			return fmt.Errorf("failed to read the virtfn%d dir of the device %q: %v", vf, ifName, err)
		}

		if (len(infos) == 0) && (vf == (vfTotal - 1)) {
			return fmt.Errorf("no Virtual function exist in directory %s, last vf is virtfn%d", vfDir, vf)
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
				return fmt.Errorf("err in getting pci address - %q", err)
			}
			break
		} else {
			return fmt.Errorf("mutiple network devices in directory %s", vfDir)
		}
	}

	// VF NIC name
	if len(infos) != 1 && len(infos) != maxSharedVf {
		return fmt.Errorf("no virutal network resources avaiable for the %q", ifName)
	}

	if conf.Sharedvf != false && conf.L2Mode != true {
		return fmt.Errorf("l2enable mode must be true to use shared net interface %q", ifName)
	}

	if conf.Vlan != 0 {
		if err = netlink.LinkSetVfVlan(m, vfIdx, conf.Vlan); err != nil {
			return fmt.Errorf("failed to set vf %d vlan: %v", vfIdx, err)
		}

		if conf.Sharedvf {
			if err = setSharedVfVlan(ifName, vfIdx, conf.Vlan); err != nil {
				return fmt.Errorf("failed to set shared vf %d vlan: %v", vfIdx, err)
			}
		}
	}

	if logFile != nil {
		logFile.Write([]byte("NETLINK: setting up transaction rate\n"))
	}

	if err = netlink.LinkSetVfTxRate(m, vfIdx, 75757); err != nil {
		return fmt.Errorf("failed to setup vf %d device: %v", vfIdx, err)
	}

	conf.DPDKConf.PCIaddr = pciAddr
	conf.DPDKConf.Ifname = podifName
	conf.DPDKConf.VFID = vfIdx
	if conf.DPDKMode != false {
		if err = saveNetConf(cid, conf.CNIDir, conf); err != nil {
			return err
		}
		return enabledpdkmode(&conf.DPDKConf, infos[0].Name(), true)
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
			return fmt.Errorf("failed to lookup vf device %q: %v", infos[i-1].Name(), err)
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
			return fmt.Errorf("failed to setup vf %d device: %v", vfIdx, err)
		}

		// move VF device to ns
		if err = netlink.LinkSetNsFd(vfDev, int(netns.Fd())); err != nil {
			return fmt.Errorf("failed to move vf %d to netns: %v", vfIdx, err)
		}
	}

	return netns.Do(func(_ ns.NetNS) error {

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

func releaseVF(conf *NetConf, podifName string, cid string, netns ns.NetNS) error {
	if logFile != nil {
		logFile.Write([]byte("ENTERING releaseVF\n"))
	}

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

		// if logFile != nil {
		// 	logFile.Write([]byte("NETLINK: change back transaction rate\n"))
		// }

		// if err = netlink.LinkSetVfTxRate(vfDev, index, 0); err != nil {
		// 	return fmt.Errorf("failed to setup vf %d device: %v", index, err)
		// }

		// shutdown VF device
		if err = netlink.LinkSetDown(vfDev); err != nil {
			return fmt.Errorf("failed to down vf device %q: %v", ifName, err)
		}

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

		//break the loop, if the namespace has no shared vf net interface
		if conf.Sharedvf != true {
			break
		}
	}

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
//	if(logFile != nil) {logFile.Write([]byte("CONTAINER ID: "))}
//	if(logFile != nil) {logFile.Write([]byte(args.ContainerID))}
//	if(logFile != nil) {logFile.Write([]byte("\nNetork Namespace: "))}
//	if(logFile != nil) {logFile.Write([]byte(args.Netns))}
	if(logFile != nil) {logFile.Write([]byte("\n"))}

	n, err := loadConf(args.StdinData)
	if err != nil {
		return fmt.Errorf("failed to load netconf: %v", err)
	}

	container_id_log, _ := os.OpenFile(fmt.Sprintf("/opt/cni/bin/%s", args.ContainerID), os.O_APPEND | os.O_CREATE | os.O_WRONLY, 0644)
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

	pfs, err := getPFs(n.IF0, n.PfNetdevices)
	if err != nil {
		return err
	}

	for _, pf := range pfs {
		err = setupVF(n, pf, args.IfName, args.ContainerID, netns)
		if err == nil {
			break
		}
	}
	defer func() {
		if err != nil {
			err = netns.Do(func(_ ns.NetNS) error {
				_, err := netlink.LinkByName(args.IfName)
				return err
			})
			if err == nil {
				releaseVF(n, args.IfName, args.ContainerID, netns)
			}
		}
	}()
	if err != nil {
		return fmt.Errorf("failed to set up pod interface %q from the device %v: %v", args.IfName, n.PfNetdevices, err)
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

	if err = releaseVF(n, args.IfName, args.ContainerID, netns); err != nil {
		return err
	}

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

func main() {
	logFile, _ = os.OpenFile("/opt/cni/bin/asdf", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	logFile.Write([]byte("ENTERING main\n"))

	config, err := clientcmd.BuildConfigFromFlags("", "/etc/kubernetes/kubelet.conf")
	if(err != nil) {
		logFile.Write([]byte("An error occured when reading config file.\n"))
	}
	clientset, err := kubernetes.NewForConfig(config)
	if(err != nil) {
		logFile.Write([]byte("An error occured when reading config file.\n"))
	}
	pods, err := clientset.CoreV1().Pods("").List(metav1.ListOptions{})
	if(err != nil) {
		logFile.Write([]byte("An error occured when reading config file.\n"))
	}
	for n := 0; n < len(pods.Items); n++ {
		logFile.Write([]byte(fmt.Sprintf("%+v", pods.Items[n].ObjectMeta)))
		logFile.Write([]byte("\n"))
	}
	logFile.Write([]byte(fmt.Sprintf("There are %d pods in the cluster\n", len(pods.Items))))

	skel.PluginMain(cmdAdd, cmdDel)
	logFile.Write([]byte("LEAVING main\n"))
	logFile.Close()
}
