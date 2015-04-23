package qemu

import (
	"fmt"
	"syscall"
	"os"
	"os/exec"
	"path"
	"strings"
    "dvm/api/docker"
    "dvm/api/pod"
    "dvm/engine"
	dm "dvm/api/storage/devicemapper"
	"dvm/api/storage/aufs"
	"dvm/lib/glog"
)

func CreateContainer(userPod *pod.UserPod, sharedDir string, hub chan QemuEvent) (string, error) {
    var (
        proto = "unix"
        addr = "/var/run/docker.sock"
		fstype string
		poolName string
		devPrefix string
		storageDriver string
		mountSharedDir string
		containerId string
		rootPath string
		devFullName string
    )
    var cli = docker.NewDockerCli("", proto, addr, nil)

	body, _, err := cli.SendCmdInfo()
	if err != nil {
		return "", err
	}
	outInfo := engine.NewOutput()
	remoteInfo, err := outInfo.AddEnv()
	if err != nil {
		return "", err
	}
	if _, err := outInfo.Write(body); err != nil {
		return "", fmt.Errorf("Error while reading remote info!\n")
	}
	outInfo.Close()
	storageDriver = remoteInfo.Get("Driver")
	if storageDriver == "devicemapper" {
		if remoteInfo.Exists("DriverStatus") {
			var driverStatus [][2]string
			if err := remoteInfo.GetJson("DriverStatus", &driverStatus); err != nil {
				return "", err
			}
			for _, pair := range driverStatus {
				if pair[0] == "Pool Name" {
					poolName = pair[1]
				}
				if pair[0] == "Backing Filesystem" {
					if strings.Contains(pair[1], "ext") {
						fstype = "ext4"
					} else if strings.Contains(pair[1], "xfs") {
						fstype = "xfs"
					} else {
						fstype = "dir"
					}
					break
				}
			}
		} else {
			// FIXME should we re-try this while encountering this error
			glog.Warning("Can not find the driver status for the devicemapper!")
		}
		devPrefix = poolName[:strings.Index(poolName, "-pool")]
		rootPath = "/var/lib/docker/devicemapper"
	} else if storageDriver == "aufs" {
		if remoteInfo.Exists("DriverStatus") {
			var driverStatus [][2]string
			if err := remoteInfo.GetJson("DriverStatus", &driverStatus); err != nil {
				return "", err
			}
			for _, pair := range driverStatus {
				if pair[0] == "Root Dir" {
					rootPath = pair[1]
				}
				if pair[0] == "Backing Filesystem" {
					if strings.Contains(pair[1], "ext") {
						fstype = "ext4"
					} else if strings.Contains(pair[1], "xfs") {
						fstype = "xfs"
					} else {
						fstype = "dir"
					}
					break
				}
			}
		} else {
			// FIXME should we re-try this while encountering this error
			glog.Warning("Can not find the driver status for the devicemapper!")
		}
	}

	// Process the 'Files' section
	files := make(map[string](pod.UserFile))
	for _, v := range userPod.Files {
		files[v.Name] = v
	}

	// Process the 'Containers' section
	fmt.Printf("Process the Containers section in POD SPEC\n")
	for i, c := range userPod.Containers {
		imgName := c.Image
		body, _, err := cli.SendCmdCreate(imgName)
		if err != nil {
			return "", err
		}
		out := engine.NewOutput()
		remoteInfo, err := out.AddEnv()
		if err != nil {
			return "", err
		}
		if _, err := out.Write(body); err != nil {
			return "", fmt.Errorf("Error while reading remote info!\n")
		}
		out.Close()

		containerId := remoteInfo.Get("Id")

		if containerId != "" {
			glog.V(1).Infof("The ContainerID is %s", containerId)
			var jsonResponse *docker.ConfigJSON
			if jsonResponse, err = cli.GetContainerInfo(containerId); err != nil {
				glog.Error("got error when get container Info ", err.Error())
				return "", err
			}

			if storageDriver == "devicemapper" {
				if err := dm.CreateNewDevice(containerId, devPrefix, rootPath); err != nil {
					return "", err
				}
				devFullName, err = dm.MountContainerToSharedDir(containerId, sharedDir, devPrefix)
				if err != nil {
					glog.Error("got error when mount container to share dir ", err.Error())
					return "", err
				}
			}
			if storageDriver == "aufs" {
				devFullName, err = aufs.MountContainerToSharedDir(containerId, rootPath, sharedDir, "")
				if err != nil {
					glog.Error("got error when mount container to share dir ", err.Error())
					return "", err
				}
			}

			for _, f := range c.Files {
				targetPath := f.Path
				fromFile := files[f.Filename].Uri
				if fromFile == "" {
					continue
				}
				if storageDriver == "devicemapper" {
					err := dm.AttachFiles(containerId, devPrefix, fromFile, targetPath, rootPath, f.Perm)
					if err != nil {
						glog.Error("got error when attach files ", err.Error())
						return "", err
					}
				}
				if storageDriver == "aufs" {
					err := aufs.AttachFiles(containerId, fromFile, targetPath, rootPath, f.Perm)
					if err != nil {
						glog.Error("got error when attach files ", err.Error())
						return "", err
					}
				}
			}

			env := make(map[string]string)
			for _, v := range jsonResponse.Config.Env {
				env[v[:strings.Index(v, "=")]] = v[strings.Index(v, "=")+1:]
			}
			glog.V(1).Infof("Parsing envs for container %d: %d Evs", i, len(env))
			glog.V(1).Infof("The fs type is %s", fstype)
            containerCreateEvent := &ContainerCreatedEvent {
                Index: i,
                Id: containerId,
                Rootfs: "/rootfs",
                Image: devFullName,
                Fstype: fstype,
                Workdir: jsonResponse.Config.WorkingDir,
                Cmd: jsonResponse.Config.Cmd,
                Envs: env,
            }
			glog.V(1).Infof("container %d created %s", i, containerId)
            hub <- containerCreateEvent
		} else {
			glog.Error("no container Id got ", err.Error())
			return "", fmt.Errorf("AN error encountered during creating container!\n")
		}
	}

	// Process the 'Volumes' section
	for _, v := range userPod.Volumes {
		if v.Source == "" {
			if storageDriver == "devicemapper" {
				volName := fmt.Sprintf("%s-volume-", devPrefix, v.Name)
				vol, err  := exec.LookPath("dmsetup")
				if err != nil {
					return "", nil
				}
				createvVolArgs := fmt.Sprintf("create %s --table \"0 %d thin %s %d\"", volName, 10737418240/512, poolName, 100)
				createVolCommand := exec.Command(vol, createvVolArgs)
				if _, err := createVolCommand.Output(); err != nil {
					return "", err
				}
				// Need to make the filesystem on that volume
				var fscmd string
				if fstype == "ext4" {
					fscmd, err = exec.LookPath("mkfs.ext4")
				} else {
					fscmd, err = exec.LookPath("mkfs.xfs")
				}
				makeFsCmd := exec.Command(fscmd, path.Join("/dev/mapper/", volName))
				if _, err := makeFsCmd.Output(); err != nil {
					return "", err
				}
				myVolReadyEvent := &VolumeReadyEvent {
					Name: v.Name,
					Filepath: path.Join("/dev/mapper/", volName),
					Fstype: fstype,
					Format: "raw",
				}
				hub <- myVolReadyEvent
				continue

			} else {
				// Make sure the v.Name is given
				v.Source = path.Join("/var/tmp/dvm/", v.Name)
				if _, err := os.Stat(v.Source); err != nil && os.IsNotExist(err) {
					if err := os.MkdirAll(v.Source, os.FileMode(0777)); err != nil {
						return "", nil
					}
				}
			}
		}

		// Process the situation if the source is not NULL, we need to bind that dir to sharedDir
		var flags uintptr = syscall.MS_MGC_VAL

		mountSharedDir = pod.RandStr(10, "alpha")
		if err := syscall.Mount(v.Source, path.Join(sharedDir, mountSharedDir), fstype, flags, "--bind"); err != nil {
			return "", nil
		}
		myVolReadyEvent := &VolumeReadyEvent {
			Name: v.Name,
			Filepath: mountSharedDir,
			Fstype: "dir",
			Format: "",
		}
		hub <- myVolReadyEvent
	}

	return containerId, nil
}

func UmountAufsContainer(shareDir, image string, index int, hub chan QemuEvent) {
	mount := path.Join(shareDir, image)
	success := true
	err := aufs.Unmount(mount)
	if err != nil {
		glog.Warningf("Cannot umount aufs %s: %s", mount, err.Error())
		success = false
	}
	hub <- &ContainerUnmounted{Index: index, Success: success}
}

func UmountVolume(shareDir, volPath string, name string, hub chan QemuEvent) {
	mount := path.Join(shareDir, volPath)
	success := true
	err := syscall.Unmount(mount, 0)
	if err != nil {
		glog.Warningf("Cannot umount volume %s: %s", mount, err.Error())
		success = false
	}
	hub <-  &VolumeUnmounted{ Name: name, Success:success,}
}