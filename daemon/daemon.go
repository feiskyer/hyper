package daemon

import (
	"fmt"
	"github.com/hyperhq/hyper/docker"
	"github.com/hyperhq/hyper/engine"
	"github.com/hyperhq/hyper/lib/portallocator"
	"github.com/hyperhq/runv/lib/glog"

	apiserver "github.com/hyperhq/hyper/server"
	dm "github.com/hyperhq/hyper/storage/devicemapper"

	"github.com/hyperhq/runv/hypervisor"
	"github.com/hyperhq/runv/hypervisor/network"
	"github.com/hyperhq/runv/hypervisor/types"

	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/Unknwon/goconfig"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/util"
)

type Storage struct {
	StorageType string
	PoolName    string
	Fstype      string
	RootPath    string
	DmPoolData  *dm.DeviceMapper
}

type Daemon struct {
	ID          string
	db          *leveldb.DB
	eng         *engine.Engine
	dockerCli   *docker.DockerCli
	podList     map[string]*hypervisor.Pod
	vmList      map[string]*hypervisor.Vm
	kernel      string
	initrd      string
	bios        string
	cbfs        string
	BridgeIface string
	BridgeIP    string
	Host        string
	Storage     *Storage
}

// Install installs daemon capabilities to eng.
func (daemon *Daemon) Install(eng *engine.Engine) error {
	// Now, we just install a command 'info' to set/get the information of the docker and Hyper daemon
	for name, method := range map[string]engine.Handler{
		"info":              daemon.CmdInfo,
		"version":           daemon.CmdVersion,
		"create":            daemon.CmdCreate,
		"pull":              daemon.CmdPull,
		"podCreate":         daemon.CmdPodCreate,
		"podStart":          daemon.CmdPodStart,
		"podInfo":           daemon.CmdPodInfo,
		"podRm":             daemon.CmdPodRm,
		"podRun":            daemon.CmdPodRun,
		"podStop":           daemon.CmdPodStop,
		"vmCreate":          daemon.CmdVmCreate,
		"vmKill":            daemon.CmdVmKill,
		"list":              daemon.CmdList,
		"exec":              daemon.CmdExec,
		"attach":            daemon.CmdAttach,
		"tty":               daemon.CmdTty,
		"serveapi":          apiserver.ServeApi,
		"acceptconnections": apiserver.AcceptConnections,
	} {
		glog.V(3).Infof("Engine Register: name= %s\n", name)
		if err := eng.Register(name, method); err != nil {
			return err
		}
	}
	return nil
}

func (daemon *Daemon) Restore() error {
	if daemon.GetPodNum() == 0 {
		return nil
	}

	podList := map[string]string{}

	iter := daemon.db.NewIterator(util.BytesPrefix([]byte("pod-")), nil)
	for iter.Next() {
		key := iter.Key()
		value := iter.Value()
		if strings.Contains(string(key), "pod-container-") {
			glog.V(1).Infof(string(value))
			continue
		}
		glog.V(1).Infof("Get the pod item, pod is %s!", key)
		err := daemon.db.Delete(key, nil)
		if err != nil {
			return err
		}
		podList[string(key)[4:]] = string(value)
	}
	iter.Release()
	err := iter.Error()
	if err != nil {
		return err
	}
	for k, v := range podList {
		err = daemon.CreatePod(k, v)
		if err != nil {
			glog.Warning("Got a unexpected error, %s", err.Error())
			continue
		}
		vmId, err := daemon.GetVmByPod(k)
		if err != nil {
			glog.V(1).Info(err.Error(), " for ", k)
			continue
		}
		daemon.podList[k].Vm = string(vmId)
	}

	// associate all VMs
	daemon.AssociateAllVms()
	return nil
}

func (daemon *Daemon) CreateVolume(podId, volName, dev_id string, restore bool) error {
	err := dm.CreateVolume(daemon.Storage.DmPoolData.PoolName, volName, dev_id, 2*1024*1024*1024, restore)
	if err != nil {
		return err
	}
	daemon.SetVolumeId(podId, volName, dev_id)
	return nil
}

func NewDaemon(eng *engine.Engine) (*Daemon, error) {
	daemon, err := NewDaemonFromDirectory(eng)
	if err != nil {
		return nil, err
	}
	return daemon, nil
}

func NewDaemonFromDirectory(eng *engine.Engine) (*Daemon, error) {
	// register portallocator release on shutdown
	eng.OnShutdown(func() {
		if err := portallocator.ReleaseAll(); err != nil {
			glog.Errorf("portallocator.ReleaseAll(): %s", err.Error())
		}
	})
	// Check that the system is supported and we have sufficient privileges
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("The Hyper daemon is only supported on linux")
	}
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("The Hyper daemon needs to be run as root")
	}
	if err := checkKernel(); err != nil {
		return nil, err
	}

	hypervisor.HDriver = DriversProbe()

	cfg, err := goconfig.LoadConfigFile(eng.Config)
	if err != nil {
		glog.Errorf("Read config file (%s) failed, %s", eng.Config, err.Error())
		return nil, err
	}
	kernel, _ := cfg.GetValue(goconfig.DEFAULT_SECTION, "Kernel")
	initrd, _ := cfg.GetValue(goconfig.DEFAULT_SECTION, "Initrd")
	glog.V(0).Infof("The config: kernel=%s, initrd=%s", kernel, initrd)
	biface, _ := cfg.GetValue(goconfig.DEFAULT_SECTION, "Bridge")
	bridgeip, _ := cfg.GetValue(goconfig.DEFAULT_SECTION, "BridgeIP")
	glog.V(0).Infof("The config: bridge=%s, ip=%s", biface, bridgeip)
	bios, _ := cfg.GetValue(goconfig.DEFAULT_SECTION, "Bios")
	cbfs, _ := cfg.GetValue(goconfig.DEFAULT_SECTION, "Cbfs")
	glog.V(0).Infof("The config: bios=%s, cbfs=%s", bios, cbfs)
	host, _ := cfg.GetValue(goconfig.DEFAULT_SECTION, "Host")

	var tempdir = "/var/run/hyper/"
	os.Setenv("TMPDIR", tempdir)
	if err := os.MkdirAll(tempdir, 0755); err != nil && !os.IsExist(err) {
		return nil, err
	}

	var realRoot = "/var/lib/hyper/"
	// Create the root directory if it doesn't exists
	if err := os.MkdirAll(realRoot, 0755); err != nil && !os.IsExist(err) {
		return nil, err
	}

	if err := network.InitNetwork(biface, bridgeip); err != nil {
		glog.Errorf("InitNetwork failed, %s\n", err.Error())
		return nil, err
	}

	var (
		proto   = "unix"
		addr    = "/var/run/docker.sock"
		db_file = fmt.Sprintf("%s/hyper.db", realRoot)
	)
	db, err := leveldb.OpenFile(db_file, nil)
	if err != nil {
		glog.Errorf("open leveldb file failed, %s\n", err.Error())
		return nil, err
	}
	dockerCli := docker.NewDockerCli("", proto, addr, nil)
	pList := map[string]*hypervisor.Pod{}
	vList := map[string]*hypervisor.Vm{}
	daemon := &Daemon{
		ID:        fmt.Sprintf("%d", os.Getpid()),
		db:        db,
		eng:       eng,
		kernel:    kernel,
		initrd:    initrd,
		bios:      bios,
		cbfs:      cbfs,
		dockerCli: dockerCli,
		podList:   pList,
		vmList:    vList,
		Host:      host,
	}

	stor := &Storage{}
	// Get the docker daemon info
	body, _, err := dockerCli.SendCmdInfo()
	if err != nil {
		return nil, err
	}
	outInfo := engine.NewOutput()
	remoteInfo, err := outInfo.AddEnv()
	if err != nil {
		return nil, err
	}
	if _, err := outInfo.Write(body); err != nil {
		return nil, fmt.Errorf("Error while reading remote info!\n")
	}
	outInfo.Close()
	storageDriver := remoteInfo.Get("Driver")
	stor.StorageType = storageDriver
	if storageDriver == "devicemapper" {
		if remoteInfo.Exists("DriverStatus") {
			var driverStatus [][2]string
			if err := remoteInfo.GetJson("DriverStatus", &driverStatus); err != nil {
				return nil, err
			}
			for _, pair := range driverStatus {
				if pair[0] == "Pool Name" {
					stor.PoolName = pair[1]
				}
				if pair[0] == "Backing Filesystem" {
					if strings.Contains(pair[1], "ext") {
						stor.Fstype = "ext4"
					} else if strings.Contains(pair[1], "xfs") {
						stor.Fstype = "xfs"
					} else {
						stor.Fstype = "dir"
					}
					break
				}
			}
		}
	} else if storageDriver == "aufs" {
		if remoteInfo.Exists("DriverStatus") {
			var driverStatus [][2]string
			if err := remoteInfo.GetJson("DriverStatus", &driverStatus); err != nil {
				return nil, err
			}
			for _, pair := range driverStatus {
				if pair[0] == "Root Dir" {
					stor.RootPath = pair[1]
				}
				if pair[0] == "Backing Filesystem" {
					stor.Fstype = "dir"
					break
				}
			}
		}
	} else if storageDriver == "overlay" {
		if remoteInfo.Exists("DriverStatus") {
			var driverStatus [][1]string
			if err := remoteInfo.GetJson("DriverStatus", &driverStatus); err != nil {
				return nil, err
			}
			for _, pair := range driverStatus {
				if pair[0] == "Backing Filesystem" {
					stor.Fstype = "dir"
					break
				}
			}
		}
		stor.RootPath = "/var/lib/docker/overlay"
	} else {
		return nil, fmt.Errorf("hyperd can not support docker's backing storage: %s", storageDriver)
	}
	daemon.Storage = stor
	dmPool := dm.DeviceMapper{
		Datafile:         "/var/lib/hyper/data",
		Metadatafile:     "/var/lib/hyper/metadata",
		DataLoopFile:     "/dev/loop6",
		MetadataLoopFile: "/dev/loop7",
		PoolName:         "hyper-volume-pool",
		Size:             20971520 * 512,
	}
	if storageDriver == "devicemapper" {
		daemon.Storage.DmPoolData = &dmPool
		// Prepare the DeviceMapper storage
		if err := dm.CreatePool(&dmPool); err != nil {
			return nil, err
		}
	} else {
		daemon.CleanVolume(0)
	}
	eng.OnShutdown(func() {
		if err := daemon.shutdown(); err != nil {
			glog.Errorf("Error during daemon.shutdown(): %v", err)
		}
	})

	return daemon, nil
}

func (daemon *Daemon) GetPodNum() int64 {
	iter := daemon.db.NewIterator(util.BytesPrefix([]byte("pod-")), nil)
	var i int64 = 0
	for iter.Next() {
		key := iter.Key()
		if strings.Contains(string(key), "pod-container-") {
			continue
		}
		i++
	}
	iter.Release()
	err := iter.Error()
	if err != nil {
		return 0
	}
	return i
}

func (daemon *Daemon) GetRunningPodNum() int64 {
	var num int64 = 0
	for _, v := range daemon.podList {
		if v.Status == types.S_POD_RUNNING {
			num++
		}
	}
	return num
}

func (daemon *Daemon) WritePodToDB(podName string, podData []byte) error {
	key := fmt.Sprintf("pod-%s", podName)
	_, err := daemon.db.Get([]byte(key), nil)
	if err != nil {
		err = daemon.db.Put([]byte(key), podData, nil)
		if err != nil {
			return err
		}
	} else {
		err = daemon.db.Delete([]byte(key), nil)
		if err != nil {
			return err
		}
		err = daemon.db.Put([]byte(key), podData, nil)
		if err != nil {
			return err
		}
	}
	return nil
}

func (daemon *Daemon) GetPodByName(podName string) ([]byte, error) {
	key := fmt.Sprintf("pod-%s", podName)
	data, err := daemon.db.Get([]byte(key), nil)
	if err != nil {
		return []byte(""), err
	}
	return data, nil
}

func (daemon *Daemon) DeletePodFromDB(podName string) error {
	key := fmt.Sprintf("pod-%s", podName)
	err := daemon.db.Delete([]byte(key), nil)
	if err != nil {
		return err
	}
	return nil
}

func (daemon *Daemon) SetVolumeId(podId, volName, dev_id string) error {
	key := fmt.Sprintf("vol-%s-%s", podId, dev_id)
	err := daemon.db.Put([]byte(key), []byte(fmt.Sprintf("%s:%s", volName, dev_id)), nil)
	if err != nil {
		return err
	}
	return nil
}

func (daemon *Daemon) GetMaxDeviceId() (int, error) {
	iter := daemon.db.NewIterator(util.BytesPrefix([]byte("vol-")), nil)
	maxId := 1
	for iter.Next() {
		value := iter.Value()
		fields := strings.Split(string(value), ":")
		id, _ := strconv.Atoi(fields[1])
		if id > maxId {
			maxId = id
		}
	}
	iter.Release()
	err := iter.Error()
	if err != nil {
		return -1, err
	}
	return maxId, nil
}

func (daemon *Daemon) GetVolumeId(podId, volName string) (int, error) {
	dev_id := 0
	key := fmt.Sprintf("vol-%s", podId)
	iter := daemon.db.NewIterator(util.BytesPrefix([]byte(key)), nil)
	for iter.Next() {
		value := iter.Value()
		fields := strings.Split(string(value), ":")
		if fields[0] == volName {
			dev_id, _ = strconv.Atoi(fields[1])
		}
	}
	iter.Release()
	err := iter.Error()
	if err != nil {
		return -1, err
	}
	return dev_id, nil
}

func (daemon *Daemon) DeleteVolumeId(podId string) error {
	key := fmt.Sprintf("vol-%s", podId)
	iter := daemon.db.NewIterator(util.BytesPrefix([]byte(key)), nil)
	for iter.Next() {
		value := iter.Key()
		if string(value)[4:18] == podId {
			fields := strings.Split(string(iter.Value()), ":")
			dev_id, _ := strconv.Atoi(fields[1])
			if err := dm.DeleteVolume(daemon.Storage.DmPoolData, dev_id); err != nil {
				glog.Error(err.Error())
				return err
			}
		}
		err := daemon.db.Delete(value, nil)
		if err != nil {
			return err
		}
	}
	iter.Release()
	err := iter.Error()
	if err != nil {
		return err
	}
	return nil
}
func (daemon *Daemon) WritePodAndContainers(podId string) error {
	key := fmt.Sprintf("pod-container-%s", podId)
	value := ""
	for _, c := range daemon.podList[podId].Containers {
		if value == "" {
			value = c.Id
		} else {
			value = value + ":" + c.Id
		}
	}
	err := daemon.db.Put([]byte(key), []byte(value), nil)
	if err != nil {
		return err
	}
	return nil
}

func (daemon *Daemon) GetPodContainersByName(podName string) ([]string, error) {
	key := fmt.Sprintf("pod-container-%s", podName)
	data, err := daemon.db.Get([]byte(key), nil)
	if err != nil {
		return nil, err
	}
	containers := strings.Split(string(data), ":")
	return containers, nil
}

func (daemon *Daemon) DeletePodContainerFromDB(podName string) error {
	key := fmt.Sprintf("pod-container-%s", podName)
	err := daemon.db.Delete([]byte(key), nil)
	if err != nil {
		return err
	}
	return nil
}

func (daemon *Daemon) GetVmByPod(podId string) (string, error) {
	key := fmt.Sprintf("vm-%s", podId)
	data, err := daemon.db.Get([]byte(key), nil)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (daemon *Daemon) UpdateVmByPod(podId, vmId string) error {
	glog.V(1).Infof("Add or Update the VM info for pod(%s)", podId)
	key := fmt.Sprintf("vm-%s", podId)
	_, err := daemon.db.Get([]byte(key), nil)
	if err == nil {
		err = daemon.db.Delete([]byte(key), nil)
		if err != nil {
			return err
		}
		err = daemon.db.Put([]byte(key), []byte(vmId), nil)
		if err != nil {
			return err
		}
	} else {
		err = daemon.db.Put([]byte(key), []byte(vmId), nil)
		if err != nil {
			return err
		}
	}
	glog.V(1).Infof("success to add or  update the VM info for pod(%s)", podId)
	return nil
}

func (daemon *Daemon) DeleteVmByPod(podId string) error {
	key := fmt.Sprintf("vm-%s", podId)
	vmId, err := daemon.db.Get([]byte(key), nil)
	if err != nil {
		return err
	}
	err = daemon.db.Delete([]byte(key), nil)
	if err != nil {
		return err
	}
	if err = daemon.DeleteVmData(string(vmId)); err != nil {
		return err
	}
	glog.V(1).Infof("success to delete the VM info for pod(%s)", podId)
	return nil
}

func (daemon *Daemon) GetPodVmByName(podName string) (string, error) {
	pod := daemon.podList[podName]
	if pod == nil {
		return "", fmt.Errorf("Not found VM for pod(%s)", podName)
	}
	return pod.Vm, nil
}

func (daemon *Daemon) GetPodByContainer(containerId string) (string, error) {
	var c *hypervisor.Container = nil

	for _, p := range daemon.podList {
		for _, c = range p.Containers {
			if c.Id == containerId {
				return p.Id, nil
			}
		}
	}

	return "", fmt.Errorf("Can not find that container!")
}

func (daemon *Daemon) AddPod(pod *hypervisor.Pod) {
	daemon.podList[pod.Id] = pod
}

func (daemon *Daemon) RemovePod(podId string) {
	delete(daemon.podList, podId)
}

func (daemon *Daemon) AddVm(vm *hypervisor.Vm) {
	daemon.vmList[vm.Id] = vm
}

func (daemon *Daemon) RemoveVm(vmId string) {
	delete(daemon.vmList, vmId)
}

func (daemon *Daemon) UpdateVmData(vmId string, data []byte) error {
	key := fmt.Sprintf("vmdata-%s", vmId)
	_, err := daemon.db.Get([]byte(key), nil)
	if err == nil {
		err = daemon.db.Delete([]byte(key), nil)
		if err != nil {
			return err
		}
		err = daemon.db.Put([]byte(key), data, nil)
		if err != nil {
			return err
		}
	}
	if err != nil && strings.Contains(err.Error(), "not found") {
		err = daemon.db.Put([]byte(key), data, nil)
		if err != nil {
			return err
		}
	}
	return err
}

func (daemon *Daemon) GetVmData(vmId string) ([]byte, error) {
	key := fmt.Sprintf("vmdata-%s", vmId)
	data, err := daemon.db.Get([]byte(key), nil)
	if err != nil {
		return []byte(""), err
	}
	return data, nil
}

func (daemon *Daemon) DeleteVmData(vmId string) error {
	key := fmt.Sprintf("vmdata-%s", vmId)
	err := daemon.db.Delete([]byte(key), nil)
	if err != nil {
		return err
	}
	return nil
}

// If the stop is 1, we do not delete the pool data. Or just delete it.
func (daemon *Daemon) CleanVolume(stop int) error {
	if daemon.Storage.StorageType == "devicemapper" {
		if stop == 0 {
			return dm.DMCleanup(daemon.Storage.DmPoolData)
		}
	}
	return nil
}

func (daemon *Daemon) DestroyAllVm() error {
	glog.V(0).Info("The daemon will stop all pod")
	for _, pod := range daemon.podList {
		daemon.StopPod(pod.Id, "yes")
	}
	iter := daemon.db.NewIterator(util.BytesPrefix([]byte("vm-")), nil)
	for iter.Next() {
		key := iter.Key()
		daemon.db.Delete(key, nil)
	}
	iter.Release()
	err := iter.Error()
	return err
}

func (daemon *Daemon) DestroyAndKeepVm() error {
	for i := 0; i < 3; i++ {
		code, err := daemon.ReleaseAllVms()
		if err != nil && code == types.E_BUSY {
			continue
		} else {
			return err
		}
	}
	return nil
}

func (daemon *Daemon) shutdown() error {
	glog.V(0).Info("The daemon will be shutdown")
	glog.V(0).Info("Shutdown all VMs")
	for vm, _ := range daemon.vmList {
		daemon.KillVm(vm)
	}
	daemon.db.Close()
	glog.Flush()
	return nil
}

// Now, the daemon can be ran for any linux kernel
func checkKernel() error {
	return nil
}