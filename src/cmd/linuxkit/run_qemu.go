package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	log "github.com/Sirupsen/logrus"
)

// QemuImg is the version of qemu container
const QemuImg = "linuxkit/aarch64/qemu:47d8f0e7191e1b5bbb366fb80e9a0ee9ab2bd01d"

// QemuConfig contains the config for Qemu
type QemuConfig struct {
	Prefix         string
	ISO            bool
	UEFI           bool
	Kernel         bool
	GUI            bool
	DiskPath       string
	DiskSize       string
	FWPath         string
	Arch           string
	CPUs           string
	Memory         string
	KVM            bool
	Containerized  bool
	QemuBinPath    string
	QemuImgPath    string
	PublishedPorts []string
}

func runQemu(args []string) {
	invoked := filepath.Base(os.Args[0])
	flags := flag.NewFlagSet("qemu", flag.ExitOnError)
	flags.Usage = func() {
		fmt.Printf("USAGE: %s run qemu [options] prefix\n\n", invoked)
		fmt.Printf("'prefix' specifies the path to the VM image.\n")
		fmt.Printf("\n")
		fmt.Printf("Options:\n")
		flags.PrintDefaults()
	}

	// Determine Flags
	enableGUI := flags.Bool("gui", false, "Set qemu to use video output instead of stdio")
	uefiBoot := flags.Bool("uefi", false, "Set UEFI boot from 'prefix'-efi.iso")
	isoBoot := flags.Bool("iso", false, "Set Legacy BIOS boot from 'prefix'.iso")
	kernelBoot := flags.Bool("kernel", true, "Set boot using 'prefix'-kernel/-initrd/-cmdline")

	// Paths and settings for Disks and UEFI firware
	disk := flags.String("disk", "", "Path to disk image to use")
	diskSz := flags.String("disk-size", "", "Size of disk to create, only created if it doesn't exist")
	fw := flags.String("fw", "/usr/share/ovmf/bios.bin", "Path to OVMF firmware for UEFI boot")

	// VM configuration
	arch := flags.String("arch", "x86_64", "Type of architecture to use, e.g. x86_64, aarch64")
	cpus := flags.String("cpus", "1", "Number of CPUs")
	mem := flags.String("mem", "1024", "Amount of memory in MB")

	publishFlags := multipleFlag{}
	flags.Var(&publishFlags, "publish", "Publish a vm's port(s) to the host (default [])")

	if err := flags.Parse(args); err != nil {
		log.Fatal("Unable to parse args")
	}
	remArgs := flags.Args()

	if len(remArgs) == 0 {
		fmt.Println("Please specify the prefix to the image to boot")
		flags.Usage()
		os.Exit(1)
	}
	prefix := remArgs[0]

	// Print warning if conflicting UEFI and ISO flags are set
	if *uefiBoot && *isoBoot {
		log.Warnf("Both -iso and -uefi have been used")
	}

	config := QemuConfig{
		Prefix:         prefix,
		ISO:            *isoBoot,
		UEFI:           *uefiBoot,
		Kernel:         *kernelBoot,
		GUI:            *enableGUI,
		DiskPath:       *disk,
		DiskSize:       *diskSz,
		FWPath:         *fw,
		Arch:           *arch,
		CPUs:           *cpus,
		Memory:         *mem,
		PublishedPorts: publishFlags,
	}

	config = discoverBackend(config)

	var err error
	if config.Containerized {
		err = runQemuContainer(config)
	} else {
		err = runQemuLocal(config)
	}
	if err != nil {
		log.Fatal(err.Error())
	}
}

func runQemuLocal(config QemuConfig) error {
	var args []string
	config, args = buildQemuCmdline(config)

	if config.DiskPath != "" {
		// If disk doesn't exist then create one
		if _, err := os.Stat(config.DiskPath); err != nil {
			if os.IsNotExist(err) {
				log.Infof("Creating new qemu disk [%s]", config.DiskPath)
				qemuImgCmd := exec.Command(config.QemuImgPath, "create", "-f", "qcow2", config.DiskPath, config.DiskSize)
				log.Debugf("%v\n", qemuImgCmd.Args)
				if err := qemuImgCmd.Run(); err != nil {
					return fmt.Errorf("Error creating disk [%s]:  %s", config.DiskPath, err.Error())
				}
			} else {
				return err
			}
		} else {
			log.Infof("Using existing disk [%s]", config.DiskPath)
		}
	}

	// Check for OVMF firmware before running
	if config.UEFI {
		if _, err := os.Stat(config.FWPath); err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("File [%s] does not exist, please ensure OVMF is installed", config.FWPath)
			}
			return err
		}
	}

	qemuCmd := exec.Command(config.QemuBinPath, args...)
	// If verbosity is enabled print out the full path/arguments
	log.Debugf("%v\n", qemuCmd.Args)

	// If we're not using a separate window then link the execution to stdin/out
	if config.GUI != true {
		qemuCmd.Stdin = os.Stdin
		qemuCmd.Stdout = os.Stdout
		qemuCmd.Stderr = os.Stderr
	}

	return qemuCmd.Run()
}

func runQemuContainer(config QemuConfig) error {
	var wd string
	if filepath.IsAbs(config.Prefix) {
		// Split the path
		wd, config.Prefix = filepath.Split(config.Prefix)
		log.Debugf("Prefix: %s", config.Prefix)
	} else {
		var err error
		wd, err = os.Getwd()
		if err != nil {
			return err
		}
	}

	var args []string
	config, args = buildQemuCmdline(config)

	dockerArgs := []string{"run", "-i", "--rm", "-v", fmt.Sprintf("%s:%s", wd, "/tmp"), "-w", "/tmp"}

	if config.KVM {
		dockerArgs = append(dockerArgs, "--device", "/dev/kvm")
	}

	if config.PublishedPorts != nil && len(config.PublishedPorts) > 0 {
		forwardings, err := buildDockerForwardings(config.PublishedPorts)
		if err != nil {
			return err
		}
		dockerArgs = append(dockerArgs, forwardings...)
	}

	dockerPath, err := exec.LookPath("docker")
	if err != nil {
		return fmt.Errorf("Unable to find docker in the $PATH")
	}

	if config.DiskPath != "" {
		// If disk doesn't exist then create one
		if _, err = os.Stat(config.DiskPath); err != nil {
			if os.IsNotExist(err) {
				log.Infof("Creating new qemu disk [%s]", config.DiskPath)
				imgArgs := append(dockerArgs, QemuImg, "qemu-img", "create", "-f", "qcow2", config.DiskPath, config.DiskSize)
				qemuImgCmd := exec.Command(dockerPath, imgArgs...)
				log.Debugf("%v\n", qemuImgCmd.Args)
				if err = qemuImgCmd.Run(); err != nil {
					return fmt.Errorf("Error creating disk [%s]:  %s", config.DiskPath, err.Error())
				}
			} else {
				return err
			}
		} else {
			log.Infof("Using existing disk [%s]", config.DiskPath)
		}
	}

	qemuArgs := append(dockerArgs, QemuImg, "qemu-system-"+config.Arch)
	qemuArgs = append(qemuArgs, args...)
	qemuCmd := exec.Command(dockerPath, qemuArgs...)
	// If verbosity is enabled print out the full path/arguments
	log.Debugf("%v\n", qemuCmd.Args)

	// GUI mode not currently supported in a container. Although it could be in future.
	if config.GUI == true {
		return fmt.Errorf("GUI mode is only supported when running locally, not in a container")
	}

	qemuCmd.Stdin = os.Stdin
	qemuCmd.Stdout = os.Stdout
	qemuCmd.Stderr = os.Stderr

	return qemuCmd.Run()
}

func buildQemuCmdline(config QemuConfig) (QemuConfig, []string) {
	// Iterate through the flags and build arguments
	var qemuArgs []string
	qemuArgs = append(qemuArgs, "-device", "virtio-rng-pci")
	qemuArgs = append(qemuArgs, "-smp", config.CPUs)
	qemuArgs = append(qemuArgs, "-m", config.Memory)

	// Look for kvm device and enable for qemu if it exists
	var err error
	if _, err = os.Stat("/dev/kvm"); os.IsNotExist(err) {
		qemuArgs = append(qemuArgs, "-machine", "virt")
	} else {
		config.KVM = true
		qemuArgs = append(qemuArgs, "-enable-kvm")
		qemuArgs = append(qemuArgs, "-machine", "virt")
	}

	if config.DiskPath != "" {
		qemuArgs = append(qemuArgs, "-drive", "file="+config.DiskPath+",format=qcow2,index=0,media=disk")
	}

	// Check flags for iso/uefi boot and if so disable kernel boot
	if config.ISO {
		config.Kernel = false
		qemuIsoPath := buildPath(config.Prefix, ".iso")
		qemuArgs = append(qemuArgs, "-cdrom", qemuIsoPath)
	}

	if config.UEFI {
		config.Kernel = false
		qemuIsoPath := buildPath(config.Prefix, "-efi.iso")
		qemuArgs = append(qemuArgs, "-pflash", config.FWPath)
		qemuArgs = append(qemuArgs, "-cdrom", qemuIsoPath)
		qemuArgs = append(qemuArgs, "-boot", "d")
	}

	// build kernel boot config from kernel/initrd/cmdline
	if config.Kernel {
		qemuKernelPath := buildPath(config.Prefix, "-kernel")
		qemuInitrdPath := buildPath(config.Prefix, "-initrd.img")
		qemuArgs = append(qemuArgs, "-kernel", qemuKernelPath)
		qemuArgs = append(qemuArgs, "-initrd", qemuInitrdPath)
		consoleString, err := ioutil.ReadFile(config.Prefix + "-cmdline")
		if err != nil {
			log.Infof(" %s\n defaulting to console output", err.Error())
			qemuArgs = append(qemuArgs, "-append", "console=ttyS0 console=tty0 page_poison=1")
		} else {
			qemuArgs = append(qemuArgs, "-append", string(consoleString))
		}
	}

	if config.PublishedPorts != nil && len(config.PublishedPorts) > 0 {
		forwardings, err := buildQemuForwardings(config.PublishedPorts, config.Containerized)
		if err != nil {
			log.Error(err)
		}
		qemuArgs = append(qemuArgs, "-net", forwardings)
		qemuArgs = append(qemuArgs, "-net", "nic")
	}

	if config.GUI != true {
		qemuArgs = append(qemuArgs, "-nographic")
	}

	return config, qemuArgs
}

func discoverBackend(config QemuConfig) QemuConfig {
	qemuBinPath := "qemu-system-" + config.Arch
	qemuImgPath := "qemu-img"

	var err error
	config.QemuBinPath, err = exec.LookPath(qemuBinPath)
	if err != nil {
		log.Infof("Unable to find %s within the $PATH. Using a container", qemuBinPath)
		config.Containerized = true
	}

	config.QemuImgPath, err = exec.LookPath(qemuImgPath)
	if err != nil {
		// No need to show the error message twice
		if !config.Containerized {
			log.Infof("Unable to find %s within the $PATH. Using a container", qemuImgPath)
			config.Containerized = true
		}
	}
	return config
}

func buildPath(prefix string, postfix string) string {
	path := prefix + postfix
	if filepath.IsAbs(path) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			log.Fatalf("File [%s] does not exist in current directory", path)
		}
	}
	return path
}

type multipleFlag []string

type publishedPorts struct {
	guest    int
	host     int
	protocol string
}

func (f *multipleFlag) String() string {
	return "A multiple flag is a type of flag that can be repeated any number of times"
}

func (f *multipleFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func splitPublish(publish string) (publishedPorts, error) {
	p := publishedPorts{}
	slice := strings.Split(publish, ":")

	if len(slice) < 2 {
		return p, fmt.Errorf("Unable to parse the ports to be published, should be in format <host>:<guest> or <host>:<guest>/<tcp|udp>")
	}

	hostPort, err := strconv.Atoi(slice[0])

	if err != nil {
		return p, fmt.Errorf("The provided hostPort can't be converted to int")
	}

	right := strings.Split(slice[1], "/")

	protocol := "tcp"
	if len(right) == 2 {
		protocol = strings.TrimSpace(strings.ToLower(right[1]))
	}

	if protocol != "tcp" && protocol != "udp" {
		return p, fmt.Errorf("Provided protocol is not valid, valid options are: udp and tcp")
	}
	guestPort, err := strconv.Atoi(right[0])

	if err != nil {
		return p, fmt.Errorf("The provided guestPort can't be converted to int")
	}

	if hostPort < 1 || hostPort > 65535 {
		return p, fmt.Errorf("Invalid hostPort: %d", hostPort)
	}

	if guestPort < 1 || guestPort > 65535 {
		return p, fmt.Errorf("Invalid guestPort: %d", guestPort)
	}

	p.guest = guestPort
	p.host = hostPort
	p.protocol = protocol
	return p, nil
}

func buildQemuForwardings(publishFlags multipleFlag, containerized bool) (string, error) {
	forwardings := "user"
	for _, publish := range publishFlags {
		p, err := splitPublish(publish)
		if err != nil {
			return "", err
		}

		hostPort := p.host
		guestPort := p.guest

		if containerized {
			hostPort = guestPort
		}
		forwardings = fmt.Sprintf("%s,hostfwd=%s::%d-:%d", forwardings, p.protocol, hostPort, guestPort)
	}

	return forwardings, nil
}

func buildDockerForwardings(publishedPorts []string) ([]string, error) {
	pmap := []string{}
	for _, port := range publishedPorts {
		s, err := splitPublish(port)
		if err != nil {
			return nil, err
		}
		pmap = append(pmap, "-p", fmt.Sprintf("%d:%d/%s", s.host, s.guest, s.protocol))
	}
	return pmap, nil
}
