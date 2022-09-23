package daemon

import (
	"bufio"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/openshift/linuxptp-daemon/pkg/pmc"

	"github.com/golang/glog"
	"k8s.io/client-go/kubernetes"

	ptpv1 "github.com/openshift/ptp-operator/api/v1"
)

const (
	PtpNamespace              = "openshift-ptp"
	PTP4L_CONF_FILE_PATH      = "/etc/ptp4l.conf"
	PTP4L_CONF_DIR            = "/ptp4l-conf"
	connectionRetryInterval   = 1 * time.Second
	eventSocket               = "/cloud-native/events.sock"
	ClockClassChangeIndicator = "selected best master clock"
)

// ProcessManager manages a set of ptpProcess
// which could be ptp4l, phc2sys or timemaster.
// Processes in ProcessManager will be started
// or stopped simultaneously.
type ProcessManager struct {
	process []*ptpProcess
}

type ptpProcess struct {
	name              string
	ifaces            []string
	ptp4lSocketPath   string
	ptp4lConfigPath   string
	ts2PhcConfigPath  string
	syncE4lConfigPath string
	configName        string
	exitCh            chan bool
	execMutex         sync.Mutex
	stopped           bool
	cmd               *exec.Cmd
}

func (p *ptpProcess) Stopped() bool {
	p.execMutex.Lock()
	me := p.stopped
	p.execMutex.Unlock()
	return me
}

func (p *ptpProcess) setStopped(val bool) {
	p.execMutex.Lock()
	p.stopped = val
	p.execMutex.Unlock()
}

// Daemon is the main structure for linuxptp instance.
// It contains all the necessary data to run linuxptp instance.
type Daemon struct {
	// node name where daemon is running
	nodeName  string
	namespace string
	// write logs to socket, this will also send metrics to the socket
	stdoutToSocket bool

	// kubeClient allows interaction with Kubernetes, including the node we are running on.
	kubeClient *kubernetes.Clientset

	ptpUpdate *LinuxPTPConfUpdate

	processManager *ProcessManager

	// channel ensure LinuxPTP.Run() exit when main function exits.
	// stopCh is created by main function and passed by Daemon via NewLinuxPTP()
	stopCh <-chan struct{}
}

// New LinuxPTP is called by daemon to generate new linuxptp instance
func New(
	nodeName string,
	namespace string,
	stdoutToSocket bool,
	kubeClient *kubernetes.Clientset,
	ptpUpdate *LinuxPTPConfUpdate,
	stopCh <-chan struct{},
) *Daemon {
	if !stdoutToSocket {
		RegisterMetrics(nodeName)
	}
	return &Daemon{
		nodeName:       nodeName,
		namespace:      namespace,
		stdoutToSocket: stdoutToSocket,
		kubeClient:     kubeClient,
		ptpUpdate:      ptpUpdate,
		processManager: &ProcessManager{},
		stopCh:         stopCh,
	}
}

// Run in a for loop to listen for any LinuxPTPConfUpdate changes
func (dn *Daemon) Run() {
	for {
		select {
		case <-dn.ptpUpdate.UpdateCh:
			err := dn.applyNodePTPProfiles()
			if err != nil {
				glog.Errorf("linuxPTP apply node profile failed: %v", err)
			}
		case <-dn.stopCh:
			for _, p := range dn.processManager.process {
				if p != nil {
					cmdStop(p)
					p = nil
				}
			}
			glog.Infof("linuxPTP stop signal received, existing..")
			return
		}
	}
}

func printWhenNotNil(p interface{}, description string) {
	switch v := p.(type) {
	case *string:
		if v != nil {
			glog.Info(description, ": ", *v)
		}
	case *int64:
		if v != nil {
			glog.Info(description, ": ", *v)
		}
	default:
		glog.Info(description, ": ", v)
	}
}

func (dn *Daemon) applyNodePTPProfiles() error {
	glog.Infof("in applyNodePTPProfiles")

	for _, p := range dn.processManager.process {
		if p != nil {
			glog.Infof("stopping process.... %+v", p)
			cmdStop(p)
			p = nil
		}
	}

	// All process should have been stopped,
	// clear process in process manager.
	// Assigning processManager.process to nil releases
	// the underlying slice to the garbage
	// collector (assuming there are no other
	// references).
	dn.processManager.process = nil

	// TODO:
	// compare nodeProfile with previous config,
	// only apply when nodeProfile changes

	glog.Infof("updating NodePTPProfiles to:")
	runID := 0
	for _, profile := range dn.ptpUpdate.NodeProfiles {
		err := dn.applyNodePtpProfile(runID, &profile)
		if err != nil {
			return err
		}
		runID++
	}

	// Start all the process
	for _, p := range dn.processManager.process {
		if p != nil {
			time.Sleep(1 * time.Second)
			go cmdRun(p, dn.stdoutToSocket)
		}
	}
	return nil
}

func (dn *Daemon) applyNodePtpProfile(runID int, nodeProfile *ptpv1.PtpProfile) error {
	socketPath := fmt.Sprintf("/var/run/ptp4l.%d.socket", runID)
	configFile := fmt.Sprintf("ptp4l.%d.config", runID)
	if nodeProfile.Ts2PhcOpts != nil {
		ts2PhcConfigFile := fmt.Sprintf("ts2phc.%d.cfg", runID)
		err := dn.addTs2PhcProfileConfig(ts2PhcConfigFile, nodeProfile)
		if err != nil {
			return fmt.Errorf("failed to add ts2phc config %s: %v", ts2PhcConfigFile, err)
		}
		configPath := fmt.Sprintf("/var/run/%s", ts2PhcConfigFile)
		err = ioutil.WriteFile(configPath, []byte(*nodeProfile.Ts2PhcConf), 0644)
		if err != nil {
			return fmt.Errorf("failed to write the configuration file named %s: %v", configPath, err)
		}
		dn.processManager.process = append(dn.processManager.process, &ptpProcess{
			name:             "ts2phc",
			ifaces:           []string{},
			configName:       ts2PhcConfigFile,
			ts2PhcConfigPath: configPath,
			exitCh:           make(chan bool),
			stopped:          false,
			cmd:              ts2phcCreateCmd(nodeProfile, configPath)})
	} else {
		glog.Infof("applyNodePtpProfile: not starting ts2phc, ts2phcOpts is empty")
	}
	if nodeProfile.SyncE4lOpts != nil {
		synce4lConfigFile := fmt.Sprintf("synce4l.%d.cfg", runID)
		err := dn.addSyncE4lProfileConfig(synce4lConfigFile, nodeProfile)
		if err != nil {
			return fmt.Errorf("failed to add synce4l config %s: %v", synce4lConfigFile, err)
		}
		configPath := fmt.Sprintf("/var/run/%s", synce4lConfigFile)
		err = ioutil.WriteFile(configPath, []byte(*nodeProfile.SyncE4lConf), 0644)
		if err != nil {
			return fmt.Errorf("failed to write the configuration file named %s: %v", configPath, err)
		}
		dn.processManager.process = append(dn.processManager.process, &ptpProcess{
			name:              "synce4l",
			ifaces:            []string{},
			configName:        synce4lConfigFile,
			syncE4lConfigPath: configPath,
			exitCh:            make(chan bool),
			stopped:           false,
			cmd:               synce4lCreateCmd(nodeProfile, configPath)})
	} else {
		glog.Infof("applyNodePtpProfile: not starting synce4l, synce4lOpts is empty")
	}

	// This will create the configuration needed to run the ptp4l and phc2sys
	err := dn.addProfileConfig(socketPath, configFile, nodeProfile)
	if err != nil {
		return fmt.Errorf("failed to add profile config %s: %v", configFile, err)
	}

	glog.Infof("------------------------------------")
	printWhenNotNil(nodeProfile.Name, "Profile Name")
	printWhenNotNil(nodeProfile.Interface, "Interface")
	printWhenNotNil(nodeProfile.Ptp4lOpts, "Ptp4lOpts")
	printWhenNotNil(nodeProfile.Ptp4lConf, "Ptp4lConf")
	printWhenNotNil(nodeProfile.SyncE4lOpts, "SyncE4lOpts")
	printWhenNotNil(nodeProfile.SyncE4lConf, "SyncE4lConf")
	printWhenNotNil(nodeProfile.Phc2sysOpts, "Phc2sysOpts")
	printWhenNotNil(nodeProfile.Ts2PhcOpts, "Ts2PhcOpts")
	printWhenNotNil(nodeProfile.Ts2PhcConf, "Ts2PhcConf")
	printWhenNotNil(nodeProfile.PtpSchedulingPolicy, "PtpSchedulingPolicy")
	printWhenNotNil(nodeProfile.PtpSchedulingPriority, "PtpSchedulingPriority")
	glog.Infof("------------------------------------")

	configPath := fmt.Sprintf("/var/run/%s", configFile)
	err = ioutil.WriteFile(configPath, []byte(*nodeProfile.Ptp4lConf), 0644)
	if err != nil {
		return fmt.Errorf("failed to write the configuration file named %s: %v", configPath, err)
	}

	dn.processManager.process = append(dn.processManager.process, &ptpProcess{
		name:            "ptp4l",
		ifaces:          strings.Split(*nodeProfile.Interface, ","),
		ptp4lConfigPath: configPath,
		ptp4lSocketPath: socketPath,
		configName:      configFile,
		exitCh:          make(chan bool),
		stopped:         false,
		cmd:             ptp4lCreateCmd(nodeProfile, configPath)})

	if nodeProfile.Phc2sysOpts != nil {
		dn.processManager.process = append(dn.processManager.process, &ptpProcess{
			name:       "phc2sys",
			ifaces:     strings.Split(*nodeProfile.Interface, ","),
			configName: configFile,
			exitCh:     make(chan bool),
			stopped:    false,
			cmd:        phc2sysCreateCmd(nodeProfile)})
	} else {
		glog.Infof("applyNodePtpProfile: not starting phc2sys, phc2sysOpts is empty")
	}
	return nil
}

func (dn *Daemon) addProfileConfig(socketPath string, configFile string, nodeProfile *ptpv1.PtpProfile) error {
	// TODO: later implement a merge capability
	if nodeProfile.Ptp4lConf == nil || *nodeProfile.Ptp4lConf == "" {
		// We need to copy this to another variable because is a pointer
		config := string(dn.ptpUpdate.defaultPTP4lConfig)
		nodeProfile.Ptp4lConf = &config
	}

	if nodeProfile.Ptp4lOpts == nil || *nodeProfile.Ptp4lOpts == "" {
		// We need to copy this to another variable because is a pointer
		opts := string("")
		nodeProfile.Ptp4lOpts = &opts
	}

	output := &ptp4lConf{}
	err := output.populatePtp4lConf(nodeProfile.Ptp4lConf)
	if err != nil {
		return err
	}

	output.profile_name = *nodeProfile.Name

	if nodeProfile.Interface != nil && *nodeProfile.Interface != "" {
		output.sections = append([]ptp4lConfSection{{options: map[string]string{}, sectionName: fmt.Sprintf("[%s]", *nodeProfile.Interface)}}, output.sections...)
	} else {
		iface := string("")
		nodeProfile.Interface = &iface
	}

	for index, section := range output.sections {
		if section.sectionName == "[global]" {
			section.options["message_tag"] = fmt.Sprintf("[%s]", configFile)
			section.options["uds_address"] = socketPath
			output.sections[index] = section
		}
	}

	// This adds the flags needed for monitor
	addFlagsForMonitor(nodeProfile, output, dn.stdoutToSocket)

	*nodeProfile.Ptp4lConf, *nodeProfile.Interface = output.renderPtp4lConf()

	if nodeProfile.Phc2sysOpts != nil {
		commandLine := fmt.Sprintf("%s -z %s -t [%s]",
			*nodeProfile.Phc2sysOpts,
			socketPath,
			configFile)
		nodeProfile.Phc2sysOpts = &commandLine
	}

	return nil
}

/*
# export ETH=enp1s0f0
# export TS2PHC_CONFIG=/home/<user>/linuxptp-3.1/configs/ts2phc-generic.cfg
# ts2phc -f $TS2PHC_CONFIG -s generic -m -c $ETH
# cat $TS2PHC_CONFIG
[global]
use_syslog 0
verbose 1
logging_level 7
ts2phc.pulsewidth 100000000
#For GNSS module
#ts2phc.nmea_serialport /dev/ttyGNSS_BBDD_0 #BB bus number DD device number /dev/
ttyGNSS_1800_0
#leapfile /../<path to .list leap second file>
[<network interface>]
ts2phc.extts_polarity rising
*/
func (dn *Daemon) addTs2PhcProfileConfig(configFile string, nodeProfile *ptpv1.PtpProfile) error {

	output := &ts2phcConf{}
	err := output.populateTs2PhcConf(nodeProfile.Ts2PhcConf)
	if err != nil {
		return err
	}

	output.profileName = *nodeProfile.Name

	for index, section := range output.sections {
		if section.sectionName == "[global]" {
			section.options["message_tag"] = fmt.Sprintf("[%s]", configFile)
			output.sections[index] = section
		}
	}
	// This adds the flags needed for monitor
	if nodeProfile.Ts2PhcOpts != nil {
		if !strings.Contains(*nodeProfile.Ts2PhcOpts, "-m") {
			glog.Info("adding -m to print messages to stdout for ts2phc to use prometheus exporter")
			*nodeProfile.Ts2PhcOpts = fmt.Sprintf("%s -m", *nodeProfile.Ts2PhcOpts)
		}
	}

	*nodeProfile.Ts2PhcConf = output.renderTs2PhcConf()

	if nodeProfile.Ts2PhcOpts != nil {
		commandLine := fmt.Sprintf("%s",
			*nodeProfile.Ts2PhcOpts)
		nodeProfile.Ts2PhcOpts = &commandLine
	}

	return nil
}

func (dn *Daemon) addSyncE4lProfileConfig(configFile string, nodeProfile *ptpv1.PtpProfile) error {

	output := &syncE4lConf{}
	err := output.populateSyncE4lConf(nodeProfile.SyncE4lConf)
	if err != nil {
		return err
	}

	output.profileName = *nodeProfile.Name

	for index, section := range output.sections {
		if section.sectionName == "[global]" {
			section.options["message_tag"] = fmt.Sprintf("[%s]", configFile)
			output.sections[index] = section
		}
	}
	// This adds the flags needed for monitor
	if nodeProfile.SyncE4lOpts != nil {
		if !strings.Contains(*nodeProfile.SyncE4lOpts, "-m") {
			glog.Info("adding -m to print messages to stdout for syncE to use prometheus exporter")
			*nodeProfile.SyncE4lOpts = fmt.Sprintf("%s -m", *nodeProfile.SyncE4lOpts)
		}
	}

	*nodeProfile.SyncE4lConf = output.renderSyncE4lConf()

	if nodeProfile.SyncE4lOpts != nil {
		commandLine := fmt.Sprintf("%s",
			*nodeProfile.SyncE4lOpts)
		nodeProfile.SyncE4lOpts = &commandLine
	}

	return nil
}

// ts2phcCreateCmd generate ts2phc command
func ts2phcCreateCmd(nodeProfile *ptpv1.PtpProfile, confFilePath string) *exec.Cmd {
	cmdLine := fmt.Sprintf("/usr/sbin/ts2phc -f %s %s",
		confFilePath,
		*nodeProfile.Ts2PhcOpts)
	cmdLine = addScheduling(nodeProfile, cmdLine)

	args := strings.Split(cmdLine, " ")
	return exec.Command(args[0], args[1:]...)
}

// synce4lCreateCmd generate synce4l command
func synce4lCreateCmd(nodeProfile *ptpv1.PtpProfile, confFilePath string) *exec.Cmd {
	cmdLine := fmt.Sprintf("/usr/sbin/synce4l -f %s %s",
		confFilePath,
		*nodeProfile.SyncE4lOpts)
	cmdLine = addScheduling(nodeProfile, cmdLine)

	args := strings.Split(cmdLine, " ")
	return exec.Command(args[0], args[1:]...)
}

// Add fifo scheduling if specified in nodeProfile
func addScheduling(nodeProfile *ptpv1.PtpProfile, cmdLine string) string {
	if nodeProfile.PtpSchedulingPolicy != nil && *nodeProfile.PtpSchedulingPolicy == "SCHED_FIFO" {
		if nodeProfile.PtpSchedulingPriority == nil {
			glog.Errorf("Priority must be set for SCHED_FIFO; using default scheduling.")
			return cmdLine
		}
		priority := *nodeProfile.PtpSchedulingPriority
		if priority < 1 || priority > 65 {
			glog.Errorf("Invalid priority %d; using default scheduling.", priority)
			return cmdLine
		}
		cmdLine = fmt.Sprintf("/bin/chrt -f %d %s", priority, cmdLine)
		glog.Infof(cmdLine)
		return cmdLine
	}
	return cmdLine
}

// phc2sysCreateCmd generate phc2sys command
func phc2sysCreateCmd(nodeProfile *ptpv1.PtpProfile) *exec.Cmd {
	cmdLine := fmt.Sprintf("/usr/sbin/phc2sys %s", *nodeProfile.Phc2sysOpts)
	cmdLine = addScheduling(nodeProfile, cmdLine)

	args := strings.Split(cmdLine, " ")
	return exec.Command(args[0], args[1:]...)
}

// ptp4lCreateCmd generate ptp4l command
func ptp4lCreateCmd(nodeProfile *ptpv1.PtpProfile, confFilePath string) *exec.Cmd {
	cmdLine := fmt.Sprintf("/usr/sbin/ptp4l -f %s %s",
		confFilePath,
		*nodeProfile.Ptp4lOpts)
	cmdLine = addScheduling(nodeProfile, cmdLine)

	args := strings.Split(cmdLine, " ")
	return exec.Command(args[0], args[1:]...)
}

func processStatus(c *net.Conn, processName, cfgName string, status int64) {
	// ptp4l[5196819.100]: [ptp4l.0.config] PTP_PROCESS_STOPPED:0/1
	deadProcessMsg := fmt.Sprintf("%s[%d]:[%s] PTP_PROCESS_STATUS:%d\n", processName, time.Now().Unix(), cfgName, status)
	UpdateProcessStatusMetrics(processName, cfgName, status)
	glog.Infof("%s\n", deadProcessMsg)
	if c == nil {
		return
	}
	_, err := (*c).Write([]byte(deadProcessMsg))
	if err != nil {
		glog.Errorf("Write error sending ptp4l/phc2sys process healths status%s:", err)
	}
}

// cmdRun runs given ptpProcess and restarts on errors
func cmdRun(p *ptpProcess, stdoutToSocket bool) {
	var c net.Conn
	done := make(chan struct{}) // Done setting up logging.  Go ahead and wait for process
	defer func() {
		if stdoutToSocket && c != nil {
			if err := c.Close(); err != nil {
				glog.Errorf("closing connection returned error %s", err)
			}
		}
		p.exitCh <- true
	}()
	for {
		glog.Infof("Starting %s...", p.name)
		glog.Infof("%s cmd: %+v", p.name, p.cmd)

		//
		// don't discard process stderr output
		//
		p.cmd.Stderr = os.Stderr
		cmdReader, err := p.cmd.StdoutPipe()
		if err != nil {
			glog.Errorf("cmdRun() error creating StdoutPipe for %s: %v", p.name, err)
			break
		}
		if !stdoutToSocket {
			scanner := bufio.NewScanner(cmdReader)
			processStatus(nil, p.name, p.configName, PtpProcessUp)
			go func() {
				for scanner.Scan() {
					output := scanner.Text()
					fmt.Printf("%s\n", output)
					extractMetrics(p.configName, p.name, p.ifaces, output)
				}
				done <- struct{}{}
			}()
		} else {
			go func() {
			connect:
				select {
				case <-p.exitCh:
					done <- struct{}{}
				default:
					c, err = net.Dial("unix", eventSocket)
					if err != nil {
						glog.Errorf("error trying to connect to event socket")
						time.Sleep(connectionRetryInterval)
						goto connect
					}
				}
				scanner := bufio.NewScanner(cmdReader)
				processStatus(&c, p.name, p.configName, PtpProcessUp)
				for scanner.Scan() {
					output := scanner.Text()
					out := fmt.Sprintf("%s\n", output)
					fmt.Printf("%s", out)
					if p.name == ptp4lProcessName {
						if strings.Contains(output, ClockClassChangeIndicator) {
							go func(c *net.Conn, cfgName string) {
								if _, matches, e := pmc.RunPMCExp(cfgName, pmc.CmdParentDataSet, pmc.ClockClassChangeRegEx); e == nil {
									//regex: 'gm.ClockClass[[:space:]]+(\d+)'
									//match  1: 'gm.ClockClass                         135'
									//match  2: '135'
									if len(matches) > 1 {
										var parseError error
										var clockClass float64
										if clockClass, parseError = strconv.ParseFloat(matches[1], 64); parseError == nil {
											glog.Infof("clock change event identified")
											//ptp4l[5196819.100]: [ptp4l.0.config] CLOCK_CLASS_CHANGE:248
											clockClassOut := fmt.Sprintf("%s[%d]:[%s] CLOCK_CLASS_CHANGE %f\n", p.name, time.Now().Unix(), p.configName, clockClass)
											fmt.Printf("%s", clockClassOut)
											_, err := (*c).Write([]byte(clockClassOut))
											if err != nil {
												glog.Errorf("failed to write class change event %s", err.Error())
											}
										} else {
											glog.Errorf("parse error in clock class value %s", parseError)
										}
									} else {
										glog.Infof("clock class change value not found via PMC")
									}
								} else {
									glog.Error("error parsing PMC util for clock class change event")
								}
							}(&c, p.configName)
						}
					}
					_, err := c.Write([]byte(out))
					if err != nil {
						glog.Errorf("Write error %s:", err)
						goto connect
					}
				}
				done <- struct{}{}
			}()
		}
		// Don't restart after termination
		if !p.Stopped() {
			err = p.cmd.Start() // this is asynchronous call,
			if err != nil {
				glog.Errorf("cmdRun() error starting %s: %v", p.name, err)
			}
		}
		<-done // goroutine is done
		err = p.cmd.Wait()
		if err != nil {
			glog.Errorf("cmdRun() error waiting for %s: %v", p.name, err)
		}
		if stdoutToSocket && c != nil {
			processStatus(&c, p.name, p.configName, PtpProcessDown)
		} else {
			processStatus(nil, p.name, p.configName, PtpProcessDown)
		}

		time.Sleep(connectionRetryInterval) // Delay to prevent flooding restarts if startup fails
		// Don't restart after termination
		if p.Stopped() {
			glog.Infof("Not recreating %s...", p.name)
			break
		} else {
			glog.Infof("Recreating %s...", p.name)
			newCmd := exec.Command(p.cmd.Args[0], p.cmd.Args[1:]...)
			p.cmd = newCmd
		}
		if stdoutToSocket && c != nil {
			if err := c.Close(); err != nil {
				glog.Errorf("closing connection returned error %s", err)
			}
		}
	}
}

// cmdStop stops ptpProcess launched by cmdRun
func cmdStop(p *ptpProcess) {
	glog.Infof("Stopping %s...", p.name)
	if p.cmd == nil {
		return
	}

	p.setStopped(true)

	if p.cmd.Process != nil {
		glog.Infof("Sending TERM to PID: %d", p.cmd.Process.Pid)
		p.cmd.Process.Signal(syscall.SIGTERM)
	}

	if p.ptp4lSocketPath != "" {
		err := os.Remove(p.ptp4lSocketPath)
		if err != nil {
			glog.Errorf("failed to remove ptp4l socket path %s: %v", p.ptp4lSocketPath, err)
		}
	}

	if p.ptp4lConfigPath != "" {
		err := os.Remove(p.ptp4lConfigPath)
		if err != nil {
			glog.Errorf("failed to remove ptp4l config path %s: %v", p.ptp4lConfigPath, err)
		}
	}
	if p.ts2PhcConfigPath != "" {
		err := os.Remove(p.ts2PhcConfigPath)
		if err != nil {
			glog.Errorf("failed to remove ts2phc config path %s: %v", p.ts2PhcConfigPath, err)
		}
	}
	if p.syncE4lConfigPath != "" {
		err := os.Remove(p.syncE4lConfigPath)
		if err != nil {
			glog.Errorf("failed to remove synce4l config path %s: %v", p.syncE4lConfigPath, err)
		}
	}
	<-p.exitCh
	glog.Infof("Process %d terminated", p.cmd.Process.Pid)
}
