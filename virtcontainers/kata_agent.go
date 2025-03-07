// Copyright (c) 2017 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

package virtcontainers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gogo/protobuf/proto"
	aTypes "github.com/kata-containers/agent/pkg/types"
	kataclient "github.com/kata-containers/agent/protocols/client"
	"github.com/kata-containers/agent/protocols/grpc"
	"github.com/kata-containers/runtime/virtcontainers/device/api"
	"github.com/kata-containers/runtime/virtcontainers/device/config"
	persistapi "github.com/kata-containers/runtime/virtcontainers/persist/api"
	vcAnnotations "github.com/kata-containers/runtime/virtcontainers/pkg/annotations"
	vccgroups "github.com/kata-containers/runtime/virtcontainers/pkg/cgroups"
	ns "github.com/kata-containers/runtime/virtcontainers/pkg/nsenter"
	"github.com/kata-containers/runtime/virtcontainers/pkg/rootless"
	vcTypes "github.com/kata-containers/runtime/virtcontainers/pkg/types"
	"github.com/kata-containers/runtime/virtcontainers/pkg/uuid"
	"github.com/kata-containers/runtime/virtcontainers/store"
	"github.com/kata-containers/runtime/virtcontainers/types"
	"github.com/opencontainers/runtime-spec/specs-go"
	opentracing "github.com/opentracing/opentracing-go"
	"github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"golang.org/x/net/context"
	"golang.org/x/sys/unix"
	golangGrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcStatus "google.golang.org/grpc/status"
)

const (
	// KataEphemeralDevType creates a tmpfs backed volume for sharing files between containers.
	KataEphemeralDevType = "ephemeral"

	// KataLocalDevType creates a local directory inside the VM for sharing files between
	// containers.
	KataLocalDevType = "local"

	// path to vfio devices
	vfioPath = "/dev/vfio/"

	agentPidEnv = "KATA_AGENT_PIDNS"
)

var (
	checkRequestTimeout         = 30 * time.Second
	defaultRequestTimeout       = 60 * time.Second
	errorMissingProxy           = errors.New("Missing proxy pointer")
	errorMissingOCISpec         = errors.New("Missing OCI specification")
	defaultKataHostSharedDir    = "/run/kata-containers/shared/sandboxes/"
	defaultKataGuestSharedDir   = "/run/kata-containers/shared/containers/"
	mountGuestTag               = "kataShared"
	defaultKataGuestSandboxDir  = "/run/kata-containers/sandbox/"
	type9pFs                    = "9p"
	typeVirtioFS                = "virtiofs"
	typeVirtioFSNoCache         = "none"
	kata9pDevType               = "9p"
	kataMmioBlkDevType          = "mmioblk"
	kataBlkDevType              = "blk"
	kataBlkCCWDevType           = "blk-ccw"
	kataSCSIDevType             = "scsi"
	kataNvdimmDevType           = "nvdimm"
	kataVirtioFSDevType         = "virtio-fs"
	sharedDir9pOptions          = []string{"trans=virtio,version=9p2000.L,cache=mmap", "nodev"}
	sharedDirVirtioFSOptions    = []string{}
	sharedDirVirtioFSDaxOptions = "dax"
	shmDir                      = "shm"
	kataEphemeralDevType        = "ephemeral"
	defaultEphemeralPath        = filepath.Join(defaultKataGuestSandboxDir, kataEphemeralDevType)
	grpcMaxDataSize             = int64(1024 * 1024)
	localDirOptions             = []string{"mode=0777"}
	maxHostnameLen              = 64
	GuestDNSFile                = "/etc/resolv.conf"
)

const (
	agentTraceModeDynamic  = "dynamic"
	agentTraceModeStatic   = "static"
	agentTraceTypeIsolated = "isolated"
	agentTraceTypeCollated = "collated"

	defaultAgentTraceMode = agentTraceModeDynamic
	defaultAgentTraceType = agentTraceTypeIsolated
)

const (
	grpcCheckRequest             = "grpc.CheckRequest"
	grpcExecProcessRequest       = "grpc.ExecProcessRequest"
	grpcCreateSandboxRequest     = "grpc.CreateSandboxRequest"
	grpcDestroySandboxRequest    = "grpc.DestroySandboxRequest"
	grpcCreateContainerRequest   = "grpc.CreateContainerRequest"
	grpcStartContainerRequest    = "grpc.StartContainerRequest"
	grpcRemoveContainerRequest   = "grpc.RemoveContainerRequest"
	grpcSignalProcessRequest     = "grpc.SignalProcessRequest"
	grpcUpdateRoutesRequest      = "grpc.UpdateRoutesRequest"
	grpcUpdateInterfaceRequest   = "grpc.UpdateInterfaceRequest"
	grpcListInterfacesRequest    = "grpc.ListInterfacesRequest"
	grpcListRoutesRequest        = "grpc.ListRoutesRequest"
	grpcAddARPNeighborsRequest   = "grpc.AddARPNeighborsRequest"
	grpcOnlineCPUMemRequest      = "grpc.OnlineCPUMemRequest"
	grpcListProcessesRequest     = "grpc.ListProcessesRequest"
	grpcUpdateContainerRequest   = "grpc.UpdateContainerRequest"
	grpcWaitProcessRequest       = "grpc.WaitProcessRequest"
	grpcTtyWinResizeRequest      = "grpc.TtyWinResizeRequest"
	grpcWriteStreamRequest       = "grpc.WriteStreamRequest"
	grpcCloseStdinRequest        = "grpc.CloseStdinRequest"
	grpcStatsContainerRequest    = "grpc.StatsContainerRequest"
	grpcPauseContainerRequest    = "grpc.PauseContainerRequest"
	grpcResumeContainerRequest   = "grpc.ResumeContainerRequest"
	grpcReseedRandomDevRequest   = "grpc.ReseedRandomDevRequest"
	grpcGuestDetailsRequest      = "grpc.GuestDetailsRequest"
	grpcMemHotplugByProbeRequest = "grpc.MemHotplugByProbeRequest"
	grpcCopyFileRequest          = "grpc.CopyFileRequest"
	grpcSetGuestDateTimeRequest  = "grpc.SetGuestDateTimeRequest"
	grpcStartTracingRequest      = "grpc.StartTracingRequest"
	grpcStopTracingRequest       = "grpc.StopTracingRequest"
	grpcGetOOMEventRequest       = "grpc.GetOOMEventRequest"
)

// The function is declared this way for mocking in unit tests
var kataHostSharedDir = func() string {
	if rootless.IsRootless() {
		// filepath.Join removes trailing slashes, but it is necessary for mounting
		return filepath.Join(rootless.GetRootlessDir(), defaultKataHostSharedDir) + "/"
	}
	return defaultKataHostSharedDir
}

// Shared path handling:
// 1. create two directories for each sandbox:
// -. /run/kata-containers/shared/sandboxes/$sbx_id/mounts/, a directory to hold all host/guest shared mounts
// -. /run/kata-containers/shared/sandboxes/$sbx_id/shared/, a host/guest shared directory (9pfs/virtiofs source dir)
//
// 2. /run/kata-containers/shared/sandboxes/$sbx_id/mounts/ is bind mounted readonly to /run/kata-containers/shared/sandboxes/$sbx_id/shared/, so guest cannot modify it
//
// 3. host-guest shared files/directories are mounted one-level under /run/kata-containers/shared/sandboxes/$sbx_id/mounts/ and thus present to guest at one level under /run/kata-containers/shared/sandboxes/$sbx_id/shared/
func getSharePath(id string) string {
	return filepath.Join(kataHostSharedDir(), id, "shared")
}

func getMountPath(id string) string {
	return filepath.Join(kataHostSharedDir(), id, "mounts")
}

func getSandboxPath(id string) string {
	return filepath.Join(kataHostSharedDir(), id)
}

// The function is declared this way for mocking in unit tests
var kataGuestSharedDir = func() string {
	if rootless.IsRootless() {
		// filepath.Join removes trailing slashes, but it is necessary for mounting
		return filepath.Join(rootless.GetRootlessDir(), defaultKataGuestSharedDir) + "/"
	}
	return defaultKataGuestSharedDir
}

// The function is declared this way for mocking in unit tests
var kataGuestSandboxDir = func() string {
	if rootless.IsRootless() {
		// filepath.Join removes trailing slashes, but it is necessary for mounting
		return filepath.Join(rootless.GetRootlessDir(), defaultKataGuestSandboxDir) + "/"
	}
	return defaultKataGuestSandboxDir
}

var kataGuestSandboxStorageDir = func() string {
	return filepath.Join(defaultKataGuestSandboxDir, "storage")
}

func ephemeralPath() string {
	if rootless.IsRootless() {
		return filepath.Join(kataGuestSandboxDir(), kataEphemeralDevType)
	}
	return defaultEphemeralPath
}

// KataAgentConfig is a structure storing information needed
// to reach the Kata Containers agent.
type KataAgentConfig struct {
	LongLiveConn      bool
	UseVSock          bool
	Debug             bool
	Trace             bool
	ContainerPipeSize uint32
	TraceMode         string
	TraceType         string
	KernelModules     []string
}

// KataAgentState is the structure describing the data stored from this
// agent implementation.
type KataAgentState struct {
	ProxyPid int
	URL      string
}

type kataAgent struct {
	shim  shim
	proxy proxy

	// lock protects the client pointer
	sync.Mutex
	client *kataclient.AgentClient

	reqHandlers    map[string]reqFunc
	state          KataAgentState
	keepConn       bool
	proxyBuiltIn   bool
	dynamicTracing bool
	dead           bool
	kmodules       []string

	vmSocket interface{}
	ctx      context.Context
}

func (k *kataAgent) trace(name string) (opentracing.Span, context.Context) {
	if k.ctx == nil {
		k.Logger().WithField("type", "bug").Error("trace called before context set")
		k.ctx = context.Background()
	}

	span, ctx := opentracing.StartSpanFromContext(k.ctx, name)

	span.SetTag("subsystem", "agent")
	span.SetTag("type", "kata")

	return span, ctx
}

func (k *kataAgent) Logger() *logrus.Entry {
	return virtLog.WithField("subsystem", "kata_agent")
}

func (k *kataAgent) longLiveConn() bool {
	return k.keepConn
}

// KataAgentSetDefaultTraceConfigOptions validates agent trace options and
// sets defaults.
func KataAgentSetDefaultTraceConfigOptions(config *KataAgentConfig) error {
	if !config.Trace {
		return nil
	}

	switch config.TraceMode {
	case agentTraceModeDynamic:
	case agentTraceModeStatic:
	case "":
		config.TraceMode = defaultAgentTraceMode
	default:
		return fmt.Errorf("invalid kata agent trace mode: %q (need %q or %q)", config.TraceMode, agentTraceModeDynamic, agentTraceModeStatic)
	}

	switch config.TraceType {
	case agentTraceTypeIsolated:
	case agentTraceTypeCollated:
	case "":
		config.TraceType = defaultAgentTraceType
	default:
		return fmt.Errorf("invalid kata agent trace type: %q (need %q or %q)", config.TraceType, agentTraceTypeIsolated, agentTraceTypeCollated)
	}

	return nil
}

// KataAgentKernelParams returns a list of Kata Agent specific kernel
// parameters.
func KataAgentKernelParams(config KataAgentConfig) []Param {
	var params []Param

	if config.Debug {
		params = append(params, Param{Key: "agent.log", Value: "debug"})
	}

	if config.Trace && config.TraceMode == agentTraceModeStatic {
		params = append(params, Param{Key: "agent.trace", Value: config.TraceType})
	}

	if config.ContainerPipeSize > 0 {
		containerPipeSize := strconv.FormatUint(uint64(config.ContainerPipeSize), 10)
		params = append(params, Param{Key: vcAnnotations.ContainerPipeSizeKernelParam, Value: containerPipeSize})
	}

	return params
}

func (k *kataAgent) handleTraceSettings(config KataAgentConfig) bool {
	if !config.Trace {
		return false
	}

	disableVMShutdown := false

	switch config.TraceMode {
	case agentTraceModeStatic:
		disableVMShutdown = true
	case agentTraceModeDynamic:
		k.dynamicTracing = true
	}

	return disableVMShutdown
}

func (k *kataAgent) init(ctx context.Context, sandbox *Sandbox, config interface{}) (disableVMShutdown bool, err error) {
	// save
	k.ctx = sandbox.ctx

	span, _ := k.trace("init")
	defer span.Finish()

	switch c := config.(type) {
	case KataAgentConfig:
		disableVMShutdown = k.handleTraceSettings(c)
		k.keepConn = c.LongLiveConn
		k.kmodules = c.KernelModules
	default:
		return false, vcTypes.ErrInvalidConfigType
	}

	k.proxy, err = newProxy(sandbox.config.ProxyType)
	if err != nil {
		return false, err
	}

	k.shim, err = newShim(sandbox.config.ShimType)
	if err != nil {
		return false, err
	}

	k.proxyBuiltIn = isProxyBuiltIn(sandbox.config.ProxyType)

	// Fetch agent runtime info.
	if useOldStore(sandbox.ctx) {
		if err := sandbox.store.Load(store.Agent, &k.state); err != nil {
			k.Logger().Debug("Could not retrieve anything from storage")
		}
	}
	return disableVMShutdown, nil
}

func (k *kataAgent) agentURL() (string, error) {
	switch s := k.vmSocket.(type) {
	case types.Socket:
		return s.HostPath, nil
	case types.VSock:
		return s.String(), nil
	case types.HybridVSock:
		return s.String(), nil
	default:
		return "", fmt.Errorf("Invalid socket type")
	}
}

func (k *kataAgent) capabilities() types.Capabilities {
	var caps types.Capabilities

	// add all capabilities supported by agent
	caps.SetBlockDeviceSupport()

	return caps
}

func (k *kataAgent) internalConfigure(h hypervisor, id string, builtin bool, config interface{}) error {
	var err error
	if config != nil {
		switch c := config.(type) {
		case KataAgentConfig:
			if k.vmSocket, err = h.generateSocket(id, c.UseVSock); err != nil {
				return err
			}
			k.keepConn = c.LongLiveConn
		default:
			return vcTypes.ErrInvalidConfigType
		}
	}

	if builtin {
		k.proxyBuiltIn = true
	}

	return nil
}

func (k *kataAgent) configure(h hypervisor, id, sharePath string, builtin bool, config interface{}) error {
	err := k.internalConfigure(h, id, builtin, config)
	if err != nil {
		return err
	}

	switch s := k.vmSocket.(type) {
	case types.Socket:
		err = h.addDevice(s, serialPortDev)
		if err != nil {
			return err
		}
	case types.VSock:
		if err = h.addDevice(s, vSockPCIDev); err != nil {
			return err
		}
	case types.HybridVSock:
		err = h.addDevice(s, hybridVirtioVsockDev)
		if err != nil {
			return err
		}
	default:
		return vcTypes.ErrInvalidConfigType
	}

	// Neither create shared directory nor add 9p device if hypervisor
	// doesn't support filesystem sharing.
	caps := h.capabilities()
	if !caps.IsFsSharingSupported() {
		return nil
	}

	// Create shared directory and add the shared volume if filesystem sharing is supported.
	// This volume contains all bind mounted container bundles.
	sharedVolume := types.Volume{
		MountTag: mountGuestTag,
		HostPath: sharePath,
	}

	if err = os.MkdirAll(sharedVolume.HostPath, DirMode); err != nil {
		return err
	}

	return h.addDevice(sharedVolume, fsDev)
}

func (k *kataAgent) configureFromGrpc(h hypervisor, id string, builtin bool, config interface{}) error {
	return k.internalConfigure(h, id, builtin, config)
}

func (k *kataAgent) setupSharedPath(sandbox *Sandbox) error {
	// create shared path structure
	sharePath := getSharePath(sandbox.id)
	mountPath := getMountPath(sandbox.id)
	if err := os.MkdirAll(sharePath, DirMode); err != nil {
		return err
	}
	if err := os.MkdirAll(mountPath, DirMode); err != nil {
		return err
	}

	// slave mount so that future mountpoints under mountPath are shown in sharePath as well
	if err := bindMount(context.Background(), mountPath, sharePath, true, "slave"); err != nil {
		return err
	}

	return nil
}

func (k *kataAgent) createSandbox(sandbox *Sandbox) error {
	span, _ := k.trace("createSandbox")
	defer span.Finish()

	if err := k.setupSharedPath(sandbox); err != nil {
		return err
	}
	return k.configure(sandbox.hypervisor, sandbox.id, getSharePath(sandbox.id), k.proxyBuiltIn, sandbox.config.AgentConfig)
}

func cmdToKataProcess(cmd types.Cmd) (process *grpc.Process, err error) {
	var i uint64
	var extraGids []uint32

	// Number of bits used to store user+group values in
	// the gRPC "User" type.
	const grpcUserBits = 32

	// User can contain only the "uid" or it can contain "uid:gid".
	parsedUser := strings.Split(cmd.User, ":")
	if len(parsedUser) > 2 {
		return nil, fmt.Errorf("cmd.User %q format is wrong", cmd.User)
	}

	i, err = strconv.ParseUint(parsedUser[0], 10, grpcUserBits)
	if err != nil {
		return nil, err
	}

	uid := uint32(i)

	var gid uint32
	if len(parsedUser) > 1 {
		i, err = strconv.ParseUint(parsedUser[1], 10, grpcUserBits)
		if err != nil {
			return nil, err
		}

		gid = uint32(i)
	}

	if cmd.PrimaryGroup != "" {
		i, err = strconv.ParseUint(cmd.PrimaryGroup, 10, grpcUserBits)
		if err != nil {
			return nil, err
		}

		gid = uint32(i)
	}

	for _, g := range cmd.SupplementaryGroups {
		var extraGid uint64

		extraGid, err = strconv.ParseUint(g, 10, grpcUserBits)
		if err != nil {
			return nil, err
		}

		extraGids = append(extraGids, uint32(extraGid))
	}

	process = &grpc.Process{
		Terminal: cmd.Interactive,
		User: grpc.User{
			UID:            uid,
			GID:            gid,
			AdditionalGids: extraGids,
		},
		Args: cmd.Args,
		Env:  cmdEnvsToStringSlice(cmd.Envs),
		Cwd:  cmd.WorkDir,
	}

	return process, nil
}

func cmdEnvsToStringSlice(ev []types.EnvVar) []string {
	var env []string

	for _, e := range ev {
		pair := []string{e.Var, e.Value}
		env = append(env, strings.Join(pair, "="))
	}

	return env
}

func (k *kataAgent) exec(sandbox *Sandbox, c Container, cmd types.Cmd) (*Process, error) {
	span, _ := k.trace("exec")
	defer span.Finish()

	var kataProcess *grpc.Process

	kataProcess, err := cmdToKataProcess(cmd)
	if err != nil {
		return nil, err
	}

	req := &grpc.ExecProcessRequest{
		ContainerId: c.id,
		ExecId:      uuid.Generate().String(),
		Process:     kataProcess,
	}

	if _, err := k.sendReq(req); err != nil {
		return nil, err
	}

	enterNSList := []ns.Namespace{
		{
			PID:  c.process.Pid,
			Type: ns.NSTypeNet,
		},
		{
			PID:  c.process.Pid,
			Type: ns.NSTypePID,
		},
	}

	return prepareAndStartShim(sandbox, k.shim, c.id, req.ExecId,
		k.state.URL, "", cmd, []ns.NSType{}, enterNSList)
}

func (k *kataAgent) updateInterface(ifc *vcTypes.Interface) (*vcTypes.Interface, error) {
	// send update interface request
	ifcReq := &grpc.UpdateInterfaceRequest{
		Interface: k.convertToKataAgentInterface(ifc),
	}
	resultingInterface, err := k.sendReq(ifcReq)
	if err != nil {
		k.Logger().WithFields(logrus.Fields{
			"interface-requested": fmt.Sprintf("%+v", ifc),
			"resulting-interface": fmt.Sprintf("%+v", resultingInterface),
		}).WithError(err).Error("update interface request failed")
	}
	if resultInterface, ok := resultingInterface.(*vcTypes.Interface); ok {
		return resultInterface, err
	}
	return nil, err
}

func (k *kataAgent) updateInterfaces(interfaces []*vcTypes.Interface) error {
	for _, ifc := range interfaces {
		if _, err := k.updateInterface(ifc); err != nil {
			return err
		}
	}
	return nil
}

func (k *kataAgent) updateRoutes(routes []*vcTypes.Route) ([]*vcTypes.Route, error) {
	if routes != nil {
		routesReq := &grpc.UpdateRoutesRequest{
			Routes: &grpc.Routes{
				Routes: k.convertToKataAgentRoutes(routes),
			},
		}
		resultingRoutes, err := k.sendReq(routesReq)
		if err != nil {
			k.Logger().WithFields(logrus.Fields{
				"routes-requested": fmt.Sprintf("%+v", routes),
				"resulting-routes": fmt.Sprintf("%+v", resultingRoutes),
			}).WithError(err).Error("update routes request failed")
		}
		resultRoutes, ok := resultingRoutes.(*grpc.Routes)
		if ok && resultRoutes != nil {
			return k.convertToRoutes(resultRoutes.Routes), err
		}
		return nil, err
	}
	return nil, nil
}

func (k *kataAgent) addARPNeighbors(neighs []*vcTypes.ARPNeighbor) error {
	if neighs != nil {
		neighsReq := &grpc.AddARPNeighborsRequest{
			Neighbors: &grpc.ARPNeighbors{
				ARPNeighbors: k.convertToKataAgentNeighbors(neighs),
			},
		}
		_, err := k.sendReq(neighsReq)
		if err != nil {
			if grpcStatus.Convert(err).Code() == codes.Unimplemented {
				k.Logger().WithFields(logrus.Fields{
					"arpneighbors-requested": fmt.Sprintf("%+v", neighs),
				}).Warn("add ARP neighbors request failed due to old agent, please upgrade Kata Containers image version")
				return nil
			}
			k.Logger().WithFields(logrus.Fields{
				"arpneighbors-requested": fmt.Sprintf("%+v", neighs),
			}).WithError(err).Error("add ARP neighbors request failed")
		}
		return err
	}
	return nil
}

func (k *kataAgent) listInterfaces() ([]*vcTypes.Interface, error) {
	req := &grpc.ListInterfacesRequest{}
	resultingInterfaces, err := k.sendReq(req)
	if err != nil {
		return nil, err
	}
	resultInterfaces, ok := resultingInterfaces.(*grpc.Interfaces)
	if !ok {
		return nil, fmt.Errorf("Unexpected type %T for interfaces", resultingInterfaces)
	}
	return k.convertToInterfaces(resultInterfaces.Interfaces)
}

func (k *kataAgent) listRoutes() ([]*vcTypes.Route, error) {
	req := &grpc.ListRoutesRequest{}
	resultingRoutes, err := k.sendReq(req)
	if err != nil {
		return nil, err
	}
	resultRoutes, ok := resultingRoutes.(*grpc.Routes)
	if !ok {
		return nil, fmt.Errorf("Unexpected type %T for routes", resultingRoutes)
	}
	return k.convertToRoutes(resultRoutes.Routes), nil
}

func (k *kataAgent) startProxy(sandbox *Sandbox) error {
	span, _ := k.trace("startProxy")
	defer span.Finish()

	var err error
	var agentURL string

	if k.proxy == nil {
		return errorMissingProxy
	}

	if k.proxy.consoleWatched() {
		return nil
	}

	if k.state.URL != "" {
		// For keepConn case, when k.state.URL isn't nil, it means shimv2 had disconnected from
		// sandbox and try to relaunch sandbox again. Here it needs to start proxy again to watch
		// the hypervisor console.
		if k.keepConn {
			agentURL = k.state.URL
		} else {
			k.Logger().WithFields(logrus.Fields{
				"sandbox":   sandbox.id,
				"proxy-pid": k.state.ProxyPid,
				"proxy-url": k.state.URL,
			}).Infof("proxy already started")
			return nil
		}
	} else {
		// Get agent socket path to provide it to the proxy.
		agentURL, err = k.agentURL()
		if err != nil {
			return err
		}
	}

	consoleURL, err := sandbox.hypervisor.getSandboxConsole(sandbox.id)
	if err != nil {
		return err
	}

	proxyParams := proxyParams{
		id:         sandbox.id,
		hid:        getHypervisorPid(sandbox.hypervisor),
		path:       sandbox.config.ProxyConfig.Path,
		agentURL:   agentURL,
		consoleURL: consoleURL,
		logger:     k.Logger().WithField("sandbox", sandbox.id),
		// Disable debug so proxy doesn't read console if we want to
		// debug the agent console ourselves.
		debug: sandbox.config.ProxyConfig.Debug &&
			!k.hasAgentDebugConsole(sandbox),
	}

	// Start the proxy here
	pid, uri, err := k.proxy.start(proxyParams)
	if err != nil {
		return err
	}

	// If error occurs after kata-proxy process start,
	// then rollback to kill kata-proxy process
	defer func() {
		if err != nil {
			k.proxy.stop(pid)
		}
	}()

	// Fill agent state with proxy information, and store them.
	if err = k.setProxy(sandbox, k.proxy, pid, uri); err != nil {
		return err
	}

	k.Logger().WithFields(logrus.Fields{
		"sandbox":   sandbox.id,
		"proxy-pid": pid,
		"proxy-url": uri,
	}).Info("proxy started")

	return nil
}

func (k *kataAgent) getAgentURL() (string, error) {
	return k.agentURL()
}

func (k *kataAgent) reuseAgent(agent agent) error {
	a, ok := agent.(*kataAgent)
	if !ok {
		return fmt.Errorf("Bug: get a wrong type of agent")
	}

	k.installReqFunc(a.client)
	k.client = a.client
	return nil
}

func (k *kataAgent) setProxy(sandbox *Sandbox, proxy proxy, pid int, url string) error {
	if url == "" {
		var err error
		if url, err = k.agentURL(); err != nil {
			return err
		}
	}

	// Are we setting the same proxy again?
	if k.proxy != nil && k.state.URL != "" && k.state.URL != url {
		k.proxy.stop(k.state.ProxyPid)
	}

	k.proxy = proxy
	k.state.ProxyPid = pid
	k.state.URL = url
	return nil
}

func (k *kataAgent) setProxyFromGrpc(proxy proxy, pid int, url string) {
	k.proxy = proxy
	k.state.ProxyPid = pid
	k.state.URL = url
}

func (k *kataAgent) getDNS(sandbox *Sandbox) ([]string, error) {
	ociSpec := sandbox.GetPatchedOCISpec()
	if ociSpec == nil {
		k.Logger().Debug("Sandbox OCI spec not found. Sandbox DNS will not be set.")
		return nil, nil
	}

	ociMounts := ociSpec.Mounts

	for _, m := range ociMounts {
		if m.Destination == GuestDNSFile {
			content, err := ioutil.ReadFile(m.Source)
			if err != nil {
				return nil, fmt.Errorf("Could not read file %s: %s", m.Source, err)
			}
			dns := strings.Split(string(content), "\n")
			return dns, nil

		}
	}
	k.Logger().Debug("DNS file not present in ociMounts. Sandbox DNS will not be set.")
	return nil, nil
}

func (k *kataAgent) startSandbox(sandbox *Sandbox) error {
	span, _ := k.trace("startSandbox")
	defer span.Finish()

	err := k.startProxy(sandbox)
	if err != nil {
		return err
	}

	defer func() {
		if err != nil {
			k.proxy.stop(k.state.ProxyPid)
		}
	}()
	hostname := sandbox.config.Hostname
	if len(hostname) > maxHostnameLen {
		hostname = hostname[:maxHostnameLen]
	}

	dns, err := k.getDNS(sandbox)
	if err != nil {
		return err
	}

	// check grpc server is serving
	if err = k.check(); err != nil {
		return err
	}

	//
	// Setup network interfaces and routes
	//
	interfaces, routes, neighs, err := generateVCNetworkStructures(sandbox.networkNS)
	if err != nil {
		return err
	}
	if err = k.updateInterfaces(interfaces); err != nil {
		return err
	}
	if _, err = k.updateRoutes(routes); err != nil {
		return err
	}
	if err = k.addARPNeighbors(neighs); err != nil {
		return err
	}

	storages := setupStorages(sandbox)

	kmodules := setupKernelModules(k.kmodules)

	req := &grpc.CreateSandboxRequest{
		Hostname:      hostname,
		Dns:           dns,
		Storages:      storages,
		SandboxPidns:  sandbox.sharePidNs,
		SandboxId:     sandbox.id,
		GuestHookPath: sandbox.config.HypervisorConfig.GuestHookPath,
		KernelModules: kmodules,
	}

	_, err = k.sendReq(req)
	if err != nil {
		return err
	}

	if k.dynamicTracing {
		_, err = k.sendReq(&grpc.StartTracingRequest{})
		if err != nil {
			return err
		}
	}

	return nil
}

func setupKernelModules(kmodules []string) []*grpc.KernelModule {
	modules := []*grpc.KernelModule{}

	for _, m := range kmodules {
		l := strings.Fields(strings.TrimSpace(m))
		if len(l) == 0 {
			continue
		}

		module := &grpc.KernelModule{Name: l[0]}
		modules = append(modules, module)
		if len(l) == 1 {
			continue
		}

		module.Parameters = append(module.Parameters, l[1:]...)
	}

	return modules
}

func setupStorages(sandbox *Sandbox) []*grpc.Storage {
	storages := []*grpc.Storage{}
	caps := sandbox.hypervisor.capabilities()

	// append 9p shared volume to storages only if filesystem sharing is supported
	if caps.IsFsSharingSupported() {
		// We mount the shared directory in a predefined location
		// in the guest.
		// This is where at least some of the host config files
		// (resolv.conf, etc...) and potentially all container
		// rootfs will reside.
		if sandbox.config.HypervisorConfig.SharedFS == config.VirtioFS {
			// If virtio-fs uses either of the two cache options 'auto, always',
			// the guest directory can be mounted with option 'dax' allowing it to
			// directly map contents from the host. When set to 'none', the mount
			// options should not contain 'dax' lest the virtio-fs daemon crashing
			// with an invalid address reference.
			if sandbox.config.HypervisorConfig.VirtioFSCache != typeVirtioFSNoCache {
				// If virtio_fs_cache_size = 0, dax should not be used.
				if sandbox.config.HypervisorConfig.VirtioFSCacheSize != 0 {
					sharedDirVirtioFSOptions = append(sharedDirVirtioFSOptions, sharedDirVirtioFSDaxOptions)
				}
			}
			sharedVolume := &grpc.Storage{
				Driver:     kataVirtioFSDevType,
				Source:     mountGuestTag,
				MountPoint: kataGuestSharedDir(),
				Fstype:     typeVirtioFS,
				Options:    sharedDirVirtioFSOptions,
			}

			storages = append(storages, sharedVolume)
		} else {
			sharedDir9pOptions = append(sharedDir9pOptions, fmt.Sprintf("msize=%d", sandbox.config.HypervisorConfig.Msize9p))

			sharedVolume := &grpc.Storage{
				Driver:     kata9pDevType,
				Source:     mountGuestTag,
				MountPoint: kataGuestSharedDir(),
				Fstype:     type9pFs,
				Options:    sharedDir9pOptions,
			}

			storages = append(storages, sharedVolume)
		}
	}

	if sandbox.shmSize > 0 {
		path := filepath.Join(kataGuestSandboxDir(), shmDir)
		shmSizeOption := fmt.Sprintf("size=%d", sandbox.shmSize)

		shmStorage := &grpc.Storage{
			Driver:     KataEphemeralDevType,
			MountPoint: path,
			Source:     "shm",
			Fstype:     "tmpfs",
			Options:    []string{"noexec", "nosuid", "nodev", "mode=1777", shmSizeOption},
		}

		storages = append(storages, shmStorage)
	}

	return storages
}

func (k *kataAgent) stopSandbox(sandbox *Sandbox) error {
	span, _ := k.trace("stopSandbox")
	defer span.Finish()

	if k.proxy == nil {
		return errorMissingProxy
	}

	req := &grpc.DestroySandboxRequest{}

	if _, err := k.sendReq(req); err != nil {
		return err
	}

	if k.dynamicTracing {
		_, err := k.sendReq(&grpc.StopTracingRequest{})
		if err != nil {
			return err
		}
	}

	if err := k.proxy.stop(k.state.ProxyPid); err != nil {
		return err
	}

	// clean up agent state
	k.state.ProxyPid = -1
	k.state.URL = ""
	return nil
}

func (k *kataAgent) replaceOCIMountSource(spec *specs.Spec, guestMounts map[string]Mount) error {
	ociMounts := spec.Mounts

	for index, m := range ociMounts {
		if guestMount, ok := guestMounts[m.Destination]; ok {
			k.Logger().Debugf("Replacing OCI mount (%s) source %s with %s", m.Destination, m.Source, guestMount.Source)
			ociMounts[index].Source = guestMount.Source
		}
	}

	return nil
}

func (k *kataAgent) removeIgnoredOCIMount(spec *specs.Spec, ignoredMounts map[string]Mount) error {
	var mounts []specs.Mount

	for _, m := range spec.Mounts {
		if _, found := ignoredMounts[m.Source]; found {
			k.Logger().WithField("removed-mount", m.Source).Debug("Removing OCI mount")
		} else {
			mounts = append(mounts, m)
		}
	}

	// Replace the OCI mounts with the updated list.
	spec.Mounts = mounts

	return nil
}

func (k *kataAgent) replaceOCIMountsForStorages(spec *specs.Spec, volumeStorages []*grpc.Storage) error {
	ociMounts := spec.Mounts
	var index int
	var m specs.Mount

	for i, v := range volumeStorages {
		for index, m = range ociMounts {
			if m.Destination != v.MountPoint {
				continue
			}

			// Create a temporary location to mount the Storage. Mounting to the correct location
			// will be handled by the OCI mount structure.
			filename := fmt.Sprintf("%s-%s", uuid.Generate().String(), filepath.Base(m.Destination))
			path := filepath.Join(kataGuestSandboxStorageDir(), filename)

			k.Logger().Debugf("Replacing OCI mount source (%s) with %s", m.Source, path)
			ociMounts[index].Source = path
			volumeStorages[i].MountPoint = path

			break
		}
		if index == len(ociMounts) {
			return fmt.Errorf("OCI mount not found for block volume %s", v.MountPoint)
		}
	}
	return nil
}

func (k *kataAgent) constraintGRPCSpec(grpcSpec *grpc.Spec, passSeccomp bool) {
	// Disable Hooks since they have been handled on the host and there is
	// no reason to send them to the agent. It would make no sense to try
	// to apply them on the guest.
	grpcSpec.Hooks = nil

	// Pass seccomp only if disable_guest_seccomp is set to false in
	// configuration.toml and guest image is seccomp capable.
	if !passSeccomp {
		grpcSpec.Linux.Seccomp = nil
	}

	// Disable SELinux inside of the virtual machine, the label will apply
	// to the KVM process
	if grpcSpec.Process.SelinuxLabel != "" {
		k.Logger().Info("SELinux label from config will be applied to the hypervisor process, not the VM workload")
		grpcSpec.Process.SelinuxLabel = ""
	}

	// By now only CPU constraints are supported
	// Issue: https://github.com/kata-containers/runtime/issues/158
	// Issue: https://github.com/kata-containers/runtime/issues/204
	grpcSpec.Linux.Resources.Devices = nil
	grpcSpec.Linux.Resources.Pids = nil
	grpcSpec.Linux.Resources.BlockIO = nil
	grpcSpec.Linux.Resources.HugepageLimits = nil
	grpcSpec.Linux.Resources.Network = nil
	if grpcSpec.Linux.Resources.CPU != nil {
		grpcSpec.Linux.Resources.CPU.Cpus = ""
		grpcSpec.Linux.Resources.CPU.Mems = ""
	}

	// There are three main reasons to do not apply systemd cgroups in the VM
	// - Initrd image doesn't have systemd.
	// - Nobody will be able to modify the resources of a specific container by using systemctl set-property.
	// - docker is not running in the VM.
	if vccgroups.IsSystemdCgroup(grpcSpec.Linux.CgroupsPath) {
		// Convert systemd cgroup to cgroupfs
		slice := strings.Split(grpcSpec.Linux.CgroupsPath, ":")
		// 0 - slice: system.slice
		// 1 - prefix: docker
		// 2 - name: abc123
		grpcSpec.Linux.CgroupsPath = filepath.Join("/", slice[1], slice[2])
	}

	// Disable network namespace since it is already handled on the host by
	// virtcontainers. The network is a complex part which cannot be simply
	// passed to the agent.
	// Every other namespaces's paths have to be emptied. This way, there
	// is no confusion from the agent, trying to find an existing namespace
	// on the guest.
	var tmpNamespaces []grpc.LinuxNamespace
	for _, ns := range grpcSpec.Linux.Namespaces {
		switch ns.Type {
		case specs.CgroupNamespace:
		case specs.NetworkNamespace:
		default:
			ns.Path = ""
			tmpNamespaces = append(tmpNamespaces, ns)
		}
	}
	grpcSpec.Linux.Namespaces = tmpNamespaces

	// VFIO char device shouldn't not appear in the guest,
	// the device driver should handle it and determinate its group.
	var linuxDevices []grpc.LinuxDevice
	for _, dev := range grpcSpec.Linux.Devices {
		if dev.Type == "c" && strings.HasPrefix(dev.Path, vfioPath) {
			k.Logger().WithField("vfio-dev", dev.Path).Debug("removing vfio device from grpcSpec")
			continue
		}
		linuxDevices = append(linuxDevices, dev)
	}
	grpcSpec.Linux.Devices = linuxDevices
}

func (k *kataAgent) handleShm(mounts []specs.Mount, sandbox *Sandbox) {
	for idx, mnt := range mounts {
		if mnt.Destination != "/dev/shm" {
			continue
		}

		// If /dev/shm for a container is backed by an ephemeral volume, skip
		// bind-mounting it to the sandbox shm.
		// A later call to handleEphemeralStorage should take care of setting up /dev/shm correctly.
		if mnt.Type == KataEphemeralDevType {
			continue
		}

		// A container shm mount is shared with sandbox shm mount.
		if sandbox.shmSize > 0 {
			mounts[idx].Type = "bind"
			mounts[idx].Options = []string{"rbind"}
			mounts[idx].Source = filepath.Join(kataGuestSandboxDir(), shmDir)
			k.Logger().WithField("shm-size", sandbox.shmSize).Info("Using sandbox shm")
		} else {
			// This should typically not happen, as a sandbox shm mount is always set up by the
			// upper stack.
			sizeOption := fmt.Sprintf("size=%d", DefaultShmSize)
			mounts[idx].Type = "tmpfs"
			mounts[idx].Source = "shm"
			mounts[idx].Options = []string{"noexec", "nosuid", "nodev", "mode=1777", sizeOption}
			k.Logger().WithField("shm-size", sizeOption).Info("Setting up a separate shm for container")
		}
	}
}

func (k *kataAgent) appendBlockDevice(dev ContainerDevice, c *Container) *grpc.Device {
	device := c.sandbox.devManager.GetDeviceByID(dev.ID)

	d, ok := device.GetDeviceInfo().(*config.BlockDrive)
	if !ok || d == nil {
		k.Logger().WithField("device", device).Error("malformed block drive")
		return nil
	}

	if d.Pmem {
		// block drive is a persistent memory device that
		// was passed as volume (-v) not as device (--device).
		// It shouldn't be visible in the container
		return nil
	}

	kataDevice := &grpc.Device{
		ContainerPath: dev.ContainerPath,
	}

	switch c.sandbox.config.HypervisorConfig.BlockDeviceDriver {
	case config.VirtioMmio:
		kataDevice.Type = kataMmioBlkDevType
		kataDevice.Id = d.VirtPath
		kataDevice.VmPath = d.VirtPath
	case config.VirtioBlockCCW:
		kataDevice.Type = kataBlkCCWDevType
		kataDevice.Id = d.DevNo
	case config.VirtioBlock:
		kataDevice.Type = kataBlkDevType
		kataDevice.Id = d.PCIPath.String()
		kataDevice.VmPath = d.VirtPath
	case config.VirtioSCSI:
		kataDevice.Type = kataSCSIDevType
		kataDevice.Id = d.SCSIAddr
	case config.Nvdimm:
		kataDevice.Type = kataNvdimmDevType
		kataDevice.VmPath = fmt.Sprintf("/dev/pmem%s", d.NvdimmID)
	}

	return kataDevice
}

func (k *kataAgent) appendVhostUserBlkDevice(dev ContainerDevice, c *Container) *grpc.Device {
	device := c.sandbox.devManager.GetDeviceByID(dev.ID)

	d, ok := device.GetDeviceInfo().(*config.VhostUserDeviceAttrs)
	if !ok || d == nil {
		k.Logger().WithField("device", device).Error("malformed vhost-user-blk drive")
		return nil
	}

	kataDevice := &grpc.Device{
		ContainerPath: dev.ContainerPath,
		Type:          kataBlkDevType,
		Id:            d.PCIPath.String(),
	}

	return kataDevice
}

func (k *kataAgent) appendDevices(deviceList []*grpc.Device, c *Container) []*grpc.Device {
	var kataDevice *grpc.Device

	for _, dev := range c.devices {
		device := c.sandbox.devManager.GetDeviceByID(dev.ID)
		if device == nil {
			k.Logger().WithField("device", dev.ID).Error("failed to find device by id")
			return nil
		}

		switch device.DeviceType() {
		case config.DeviceBlock:
			kataDevice = k.appendBlockDevice(dev, c)
		case config.VhostUserBlk:
			kataDevice = k.appendVhostUserBlkDevice(dev, c)
		}

		if kataDevice == nil {
			continue
		}

		deviceList = append(deviceList, kataDevice)
	}

	return deviceList
}

// rollbackFailingContainerCreation rolls back important steps that might have
// been performed before the container creation failed.
// - Unmount container volumes.
// - Unmount container rootfs.
func (k *kataAgent) rollbackFailingContainerCreation(c *Container) {
	if c != nil {
		if err2 := c.unmountHostMounts(); err2 != nil {
			k.Logger().WithError(err2).Error("rollback failed unmountHostMounts()")
		}

		if err2 := bindUnmountContainerRootfs(k.ctx, getMountPath(c.sandbox.id), c); err2 != nil {
			k.Logger().WithError(err2).Error("rollback failed bindUnmountContainerRootfs()")
		}
	}
}

func (k *kataAgent) buildContainerRootfs(sandbox *Sandbox, c *Container, rootPathParent string) (*grpc.Storage, error) {
	if c.state.Fstype != "" && c.state.BlockDeviceID != "" {
		// The rootfs storage volume represents the container rootfs
		// mount point inside the guest.
		// It can be a block based device (when using block based container
		// overlay on the host) mount or a 9pfs one (for all other overlay
		// implementations).
		rootfs := &grpc.Storage{}

		// This is a block based device rootfs.
		device := sandbox.devManager.GetDeviceByID(c.state.BlockDeviceID)
		if device == nil {
			k.Logger().WithField("device", c.state.BlockDeviceID).Error("failed to find device by id")
			return nil, fmt.Errorf("failed to find device by id %q", c.state.BlockDeviceID)
		}

		blockDrive, ok := device.GetDeviceInfo().(*config.BlockDrive)
		if !ok || blockDrive == nil {
			k.Logger().Error("malformed block drive")
			return nil, fmt.Errorf("malformed block drive")
		}
		switch {
		case sandbox.config.HypervisorConfig.BlockDeviceDriver == config.VirtioMmio:
			rootfs.Driver = kataMmioBlkDevType
			rootfs.Source = blockDrive.VirtPath
		case sandbox.config.HypervisorConfig.BlockDeviceDriver == config.VirtioBlockCCW:
			rootfs.Driver = kataBlkCCWDevType
			rootfs.Source = blockDrive.DevNo
		case sandbox.config.HypervisorConfig.BlockDeviceDriver == config.VirtioBlock:
			rootfs.Driver = kataBlkDevType
			if blockDrive.PCIPath.IsNil() {
				rootfs.Source = blockDrive.VirtPath
			} else {
				rootfs.Source = blockDrive.PCIPath.String()
			}

		case sandbox.config.HypervisorConfig.BlockDeviceDriver == config.VirtioSCSI:
			rootfs.Driver = kataSCSIDevType
			rootfs.Source = blockDrive.SCSIAddr
		default:
			return nil, fmt.Errorf("Unknown block device driver: %s", sandbox.config.HypervisorConfig.BlockDeviceDriver)
		}

		rootfs.MountPoint = rootPathParent
		rootfs.Fstype = c.state.Fstype

		if c.state.Fstype == "xfs" {
			rootfs.Options = []string{"nouuid"}
		}

		// Ensure container mount destination exists
		// TODO: remove dependency on shared fs path. shared fs is just one kind of storage source.
		// we should not always use shared fs path for all kinds of storage. Instead, all storage
		// should be bind mounted to a tmpfs path for containers to use.
		if err := os.MkdirAll(filepath.Join(getMountPath(c.sandbox.id), c.id, c.rootfsSuffix), DirMode); err != nil {
			return nil, err
		}
		return rootfs, nil
	}

	// This is not a block based device rootfs. We are going to bind mount it into the shared drive
	// between the host and the guest.
	// With virtiofs/9pfs we don't need to ask the agent to mount the rootfs as the shared directory
	// (kataGuestSharedDir) is already mounted in the guest. We only need to mount the rootfs from
	// the host and it will show up in the guest.
	if err := bindMountContainerRootfs(k.ctx, getMountPath(sandbox.id), c.id, c.rootFs.Target, false); err != nil {
		return nil, err
	}

	return nil, nil
}

func (k *kataAgent) hasAgentDebugConsole(sandbox *Sandbox) bool {
	for _, p := range sandbox.config.HypervisorConfig.KernelParams {
		if p.Key == "agent.debug_console" {
			k.Logger().Info("agent has debug console")
			return true
		}
	}
	return false
}

func (k *kataAgent) createContainer(sandbox *Sandbox, c *Container) (p *Process, err error) {
	span, _ := k.trace("createContainer")
	defer span.Finish()

	var ctrStorages []*grpc.Storage
	var ctrDevices []*grpc.Device
	var rootfs *grpc.Storage

	// This is the guest absolute root path for that container.
	rootPathParent := filepath.Join(kataGuestSharedDir(), c.id)
	rootPath := filepath.Join(rootPathParent, c.rootfsSuffix)

	// In case the container creation fails, the following defer statement
	// takes care of rolling back actions previously performed.
	defer func() {
		if err != nil {
			k.Logger().WithError(err).Error("createContainer failed")
			k.rollbackFailingContainerCreation(c)
		}
	}()

	// setup rootfs -- if its block based, we'll receive a non-nil storage object representing
	// the block device for the rootfs, which us utilized for mounting in the guest. This'll be handled
	// already for non-block based rootfs
	if rootfs, err = k.buildContainerRootfs(sandbox, c, rootPathParent); err != nil {
		return nil, err
	}

	if rootfs != nil {
		// Add rootfs to the list of container storage.
		// We only need to do this for block based rootfs, as we
		// want the agent to mount it into the right location
		// (kataGuestSharedDir/ctrID/
		ctrStorages = append(ctrStorages, rootfs)
	}

	ociSpec := c.GetPatchedOCISpec()
	if ociSpec == nil {
		return nil, errorMissingOCISpec
	}

	// Handle container mounts
	newMounts, ignoredMounts, err := c.mountSharedDirMounts(getSharePath(sandbox.id), getMountPath(sandbox.id), kataGuestSharedDir())
	if err != nil {
		return nil, err
	}

	k.handleShm(ociSpec.Mounts, sandbox)

	epheStorages := k.handleEphemeralStorage(ociSpec.Mounts)
	ctrStorages = append(ctrStorages, epheStorages...)

	localStorages := k.handleLocalStorage(ociSpec.Mounts, sandbox.id, c.rootfsSuffix)
	ctrStorages = append(ctrStorages, localStorages...)

	// We replace all OCI mount sources that match our container mount
	// with the right source path (The guest one).
	if err = k.replaceOCIMountSource(ociSpec, newMounts); err != nil {
		return nil, err
	}

	// Remove all mounts that should be ignored from the spec
	if err = k.removeIgnoredOCIMount(ociSpec, ignoredMounts); err != nil {
		return nil, err
	}

	// Append container devices for block devices passed with --device.
	ctrDevices = k.appendDevices(ctrDevices, c)

	// Handle all the volumes that are block device files.
	// Note this call modifies the list of container devices to make sure
	// all hotplugged devices are unplugged, so this needs be done
	// after devices passed with --device are handled.
	volumeStorages, err := k.handleBlockVolumes(c)
	if err != nil {
		return nil, err
	}

	if err := k.replaceOCIMountsForStorages(ociSpec, volumeStorages); err != nil {
		return nil, err
	}

	ctrStorages = append(ctrStorages, volumeStorages...)

	grpcSpec, err := grpc.OCItoGRPC(ociSpec)
	if err != nil {
		return nil, err
	}

	// We need to give the OCI spec our absolute rootfs path in the guest.
	grpcSpec.Root.Path = rootPath

	sharedPidNs := k.handlePidNamespace(grpcSpec, sandbox)

	agentPidNs := k.checkAgentPidNs(c)
	if agentPidNs {
		if !sandbox.config.EnableAgentPidNs {
			agentPidNs = false
			k.Logger().Warn("Env variable for sharing container pid namespace with the agent set, but the runtime configuration does not allow this")
		} else {
			k.Logger().Warn("Container will share PID namespace with the agent")
		}
	}
	passSeccomp := !sandbox.config.DisableGuestSeccomp && sandbox.seccompSupported

	// We need to constraint the spec to make sure we're not passing
	// irrelevant information to the agent.
	k.constraintGRPCSpec(grpcSpec, passSeccomp)

	req := &grpc.CreateContainerRequest{
		ContainerId:  c.id,
		ExecId:       c.id,
		Storages:     ctrStorages,
		Devices:      ctrDevices,
		OCI:          grpcSpec,
		SandboxPidns: sharedPidNs,
		AgentPidns:   agentPidNs,
	}

	if _, err = k.sendReq(req); err != nil {
		return nil, err
	}

	createNSList := []ns.NSType{ns.NSTypePID}

	enterNSList := []ns.Namespace{}
	if sandbox.networkNS.NetNsPath != "" {
		enterNSList = append(enterNSList, ns.Namespace{
			Path: sandbox.networkNS.NetNsPath,
			Type: ns.NSTypeNet,
		})
	}

	// Ask to the shim to print the agent logs, if it's the process who monitors the sandbox and use_vsock is true (no proxy)
	// Don't read the console socket if agent debug console is enabled.
	var consoleURL string
	if sandbox.config.HypervisorConfig.UseVSock &&
		c.GetAnnotations()[vcAnnotations.ContainerTypeKey] == string(PodSandbox) &&
		!k.hasAgentDebugConsole(sandbox) {
		consoleURL, err = sandbox.hypervisor.getSandboxConsole(sandbox.id)
		if err != nil {
			return nil, err
		}
	}

	return prepareAndStartShim(sandbox, k.shim, c.id, req.ExecId,
		k.state.URL, consoleURL, c.config.Cmd, createNSList, enterNSList)
}

// handleEphemeralStorage handles ephemeral storages by
// creating a Storage from corresponding source of the mount point
func (k *kataAgent) handleEphemeralStorage(mounts []specs.Mount) []*grpc.Storage {
	var epheStorages []*grpc.Storage
	for idx, mnt := range mounts {
		if mnt.Type == KataEphemeralDevType {
			// Set the mount source path to a path that resides inside the VM
			mounts[idx].Source = filepath.Join(ephemeralPath(), filepath.Base(mnt.Source))
			// Set the mount type to "bind"
			mounts[idx].Type = "bind"

			// Create a storage struct so that kata agent is able to create
			// tmpfs backed volume inside the VM
			epheStorage := &grpc.Storage{
				Driver:     KataEphemeralDevType,
				Source:     "tmpfs",
				Fstype:     "tmpfs",
				MountPoint: mounts[idx].Source,
			}
			epheStorages = append(epheStorages, epheStorage)
		}
	}
	return epheStorages
}

// handleLocalStorage handles local storage within the VM
// by creating a directory in the VM from the source of the mount point.
func (k *kataAgent) handleLocalStorage(mounts []specs.Mount, sandboxID string, rootfsSuffix string) []*grpc.Storage {
	var localStorages []*grpc.Storage
	for idx, mnt := range mounts {
		if mnt.Type == KataLocalDevType {
			// Set the mount source path to a the desired directory point in the VM.
			// In this case it is located in the sandbox directory.
			// We rely on the fact that the first container in the VM has the same ID as the sandbox ID.
			// In Kubernetes, this is usually the pause container and we depend on it existing for
			// local directories to work.
			mounts[idx].Source = filepath.Join(kataGuestSharedDir(), sandboxID, rootfsSuffix, KataLocalDevType, filepath.Base(mnt.Source))

			// Create a storage struct so that the kata agent is able to create the
			// directory inside the VM.
			localStorage := &grpc.Storage{
				Driver:     KataLocalDevType,
				Source:     KataLocalDevType,
				Fstype:     KataLocalDevType,
				MountPoint: mounts[idx].Source,
				Options:    localDirOptions,
			}
			localStorages = append(localStorages, localStorage)
		}
	}
	return localStorages
}

// handleDeviceBlockVolume handles volume that is block device file
// and DeviceBlock type.
func (k *kataAgent) handleDeviceBlockVolume(c *Container, m Mount, device api.Device) (*grpc.Storage, error) {
	vol := &grpc.Storage{}

	blockDrive, ok := device.GetDeviceInfo().(*config.BlockDrive)
	if !ok || blockDrive == nil {
		k.Logger().Error("malformed block drive")
		return nil, fmt.Errorf("malformed block drive")
	}
	switch {
	// pmem volumes case
	case blockDrive.Pmem:
		vol.Driver = kataNvdimmDevType
		vol.Source = fmt.Sprintf("/dev/pmem%s", blockDrive.NvdimmID)
		vol.Fstype = blockDrive.Format
		vol.Options = []string{"dax"}
	case c.sandbox.config.HypervisorConfig.BlockDeviceDriver == config.VirtioBlockCCW:
		vol.Driver = kataBlkCCWDevType
		vol.Source = blockDrive.DevNo
	case c.sandbox.config.HypervisorConfig.BlockDeviceDriver == config.VirtioBlock:
		vol.Driver = kataBlkDevType
		if blockDrive.PCIPath.IsNil() {
			vol.Source = blockDrive.VirtPath
		} else {
			vol.Source = blockDrive.PCIPath.String()
		}
	case c.sandbox.config.HypervisorConfig.BlockDeviceDriver == config.VirtioMmio:
		vol.Driver = kataMmioBlkDevType
		vol.Source = blockDrive.VirtPath
	case c.sandbox.config.HypervisorConfig.BlockDeviceDriver == config.VirtioSCSI:
		vol.Driver = kataSCSIDevType
		vol.Source = blockDrive.SCSIAddr
	default:
		return nil, fmt.Errorf("Unknown block device driver: %s", c.sandbox.config.HypervisorConfig.BlockDeviceDriver)
	}

	vol.MountPoint = m.Destination

	// If no explicit FS Type or Options are being set, then let's use what is provided for the particular mount:
	if vol.Fstype == "" {
		vol.Fstype = m.Type
	}
	if len(vol.Options) == 0 {
		vol.Options = m.Options
	}

	return vol, nil
}

// handleVhostUserBlkVolume handles volume that is block device file
// and VhostUserBlk type.
func (k *kataAgent) handleVhostUserBlkVolume(c *Container, m Mount, device api.Device) (*grpc.Storage, error) {
	vol := &grpc.Storage{}

	d, ok := device.GetDeviceInfo().(*config.VhostUserDeviceAttrs)
	if !ok || d == nil {
		k.Logger().Error("malformed vhost-user blk drive")
		return nil, fmt.Errorf("malformed vhost-user blk drive")
	}

	vol.Driver = kataBlkDevType
	vol.Source = d.PCIPath.String()
	vol.Fstype = "bind"
	vol.Options = []string{"bind"}
	vol.MountPoint = m.Destination

	return vol, nil
}

// handleBlockVolumes handles volumes that are block devices files
// by passing the block devices as Storage to the agent.
func (k *kataAgent) handleBlockVolumes(c *Container) ([]*grpc.Storage, error) {

	var volumeStorages []*grpc.Storage

	for _, m := range c.mounts {
		id := m.BlockDeviceID

		if len(id) == 0 {
			continue
		}

		// Add the block device to the list of container devices, to make sure the
		// device is detached with detachDevices() for a container.
		c.devices = append(c.devices, ContainerDevice{ID: id, ContainerPath: m.Destination})

		var vol *grpc.Storage

		device := c.sandbox.devManager.GetDeviceByID(id)
		if device == nil {
			k.Logger().WithField("device", id).Error("failed to find device by id")
			return nil, fmt.Errorf("Failed to find device by id (id=%s)", id)
		}

		var err error
		switch device.DeviceType() {
		case config.DeviceBlock:
			vol, err = k.handleDeviceBlockVolume(c, m, device)
		case config.VhostUserBlk:
			vol, err = k.handleVhostUserBlkVolume(c, m, device)
		default:
			k.Logger().Error("Unknown device type")
			continue
		}

		if vol == nil || err != nil {
			return nil, err
		}

		volumeStorages = append(volumeStorages, vol)
	}

	return volumeStorages, nil
}

// handlePidNamespace checks if Pid namespace for a container needs to be shared with its sandbox
// pid namespace. This function also modifies the grpc spec to remove the pid namespace
// from the list of namespaces passed to the agent.
func (k *kataAgent) handlePidNamespace(grpcSpec *grpc.Spec, sandbox *Sandbox) bool {
	sharedPidNs := false
	pidIndex := -1

	for i, ns := range grpcSpec.Linux.Namespaces {
		if ns.Type != string(specs.PIDNamespace) {
			continue
		}

		pidIndex = i
		// host pidns path does not make sense in kata. Let's just align it with
		// sandbox namespace whenever it is set.
		if ns.Path != "" {
			sharedPidNs = true
		}
		break
	}

	// Remove pid namespace.
	if pidIndex >= 0 {
		grpcSpec.Linux.Namespaces = append(grpcSpec.Linux.Namespaces[:pidIndex], grpcSpec.Linux.Namespaces[pidIndex+1:]...)
	}

	return sharedPidNs
}

// checkAgentPidNs checks if environment variable KATA_AGENT_PIDNS has been set for a containers
// This variable is used to indicate if the containers pid namespace should be shared
// with the agent pidns. This approach was taken due to the lack of support for container level annotations.
func (k *kataAgent) checkAgentPidNs(container *Container) bool {
	agentPidNs := false

	for _, env := range container.config.Cmd.Envs {
		if env.Var == agentPidEnv {
			if val, err := strconv.ParseBool(env.Value); err == nil {
				agentPidNs = val
			}
		}
	}

	return agentPidNs
}

func (k *kataAgent) startContainer(sandbox *Sandbox, c *Container) error {
	span, _ := k.trace("startContainer")
	defer span.Finish()

	req := &grpc.StartContainerRequest{
		ContainerId: c.id,
	}

	_, err := k.sendReq(req)
	return err
}

func (k *kataAgent) stopContainer(sandbox *Sandbox, c Container) error {
	span, _ := k.trace("stopContainer")
	defer span.Finish()

	_, err := k.sendReq(&grpc.RemoveContainerRequest{ContainerId: c.id})
	return err
}

func (k *kataAgent) signalProcess(c *Container, processID string, signal syscall.Signal, all bool) error {
	execID := processID
	if all {
		// kata agent uses empty execId to signal all processes in a container
		execID = ""
	}
	req := &grpc.SignalProcessRequest{
		ContainerId: c.id,
		ExecId:      execID,
		Signal:      uint32(signal),
	}

	_, err := k.sendReq(req)
	return err
}

func (k *kataAgent) winsizeProcess(c *Container, processID string, height, width uint32) error {
	req := &grpc.TtyWinResizeRequest{
		ContainerId: c.id,
		ExecId:      processID,
		Row:         height,
		Column:      width,
	}

	_, err := k.sendReq(req)
	return err
}

func (k *kataAgent) processListContainer(sandbox *Sandbox, c Container, options ProcessListOptions) (ProcessList, error) {
	req := &grpc.ListProcessesRequest{
		ContainerId: c.id,
		Format:      options.Format,
		Args:        options.Args,
	}

	resp, err := k.sendReq(req)
	if err != nil {
		return nil, err
	}

	processList, ok := resp.(*grpc.ListProcessesResponse)
	if !ok {
		return nil, fmt.Errorf("Bad list processes response")
	}

	return processList.ProcessList, nil
}

func (k *kataAgent) updateContainer(sandbox *Sandbox, c Container, resources specs.LinuxResources) error {
	grpcResources, err := grpc.ResourcesOCItoGRPC(&resources)
	if err != nil {
		return err
	}

	req := &grpc.UpdateContainerRequest{
		ContainerId: c.id,
		Resources:   grpcResources,
	}

	_, err = k.sendReq(req)
	return err
}

func (k *kataAgent) pauseContainer(sandbox *Sandbox, c Container) error {
	req := &grpc.PauseContainerRequest{
		ContainerId: c.id,
	}

	_, err := k.sendReq(req)
	return err
}

func (k *kataAgent) resumeContainer(sandbox *Sandbox, c Container) error {
	req := &grpc.ResumeContainerRequest{
		ContainerId: c.id,
	}

	_, err := k.sendReq(req)
	return err
}

func (k *kataAgent) memHotplugByProbe(addr uint64, sizeMB uint32, memorySectionSizeMB uint32) error {
	if memorySectionSizeMB == uint32(0) {
		return fmt.Errorf("memorySectionSizeMB couldn't be zero")
	}
	// hot-added memory device should be sliced into the size of memory section, which is the basic unit for
	// memory hotplug
	numSection := uint64(sizeMB / memorySectionSizeMB)
	var addrList []uint64
	index := uint64(0)
	for index < numSection {
		k.Logger().WithFields(logrus.Fields{
			"addr": fmt.Sprintf("0x%x", addr+(index*uint64(memorySectionSizeMB))<<20),
		}).Debugf("notify guest kernel the address of memory device")
		addrList = append(addrList, addr+(index*uint64(memorySectionSizeMB))<<20)
		index++
	}
	req := &grpc.MemHotplugByProbeRequest{
		MemHotplugProbeAddr: addrList,
	}

	_, err := k.sendReq(req)
	return err
}

func (k *kataAgent) onlineCPUMem(cpus uint32, cpuOnly bool) error {
	req := &grpc.OnlineCPUMemRequest{
		Wait:    false,
		NbCpus:  cpus,
		CpuOnly: cpuOnly,
	}

	_, err := k.sendReq(req)
	return err
}

func (k *kataAgent) statsContainer(sandbox *Sandbox, c Container) (*ContainerStats, error) {
	req := &grpc.StatsContainerRequest{
		ContainerId: c.id,
	}

	returnStats, err := k.sendReq(req)

	if err != nil {
		return nil, err
	}

	stats, ok := returnStats.(*grpc.StatsContainerResponse)
	if !ok {
		return nil, fmt.Errorf("irregular response container stats")
	}

	data, err := json.Marshal(stats.CgroupStats)
	if err != nil {
		return nil, err
	}

	var cgroupStats CgroupStats
	err = json.Unmarshal(data, &cgroupStats)
	if err != nil {
		return nil, err
	}
	containerStats := &ContainerStats{
		CgroupStats: &cgroupStats,
	}
	return containerStats, nil
}

func (k *kataAgent) connect() error {
	if k.dead {
		return errors.New("Dead agent")
	}
	// lockless quick pass
	if k.client != nil {
		return nil
	}

	span, _ := k.trace("connect")
	defer span.Finish()

	// This is for the first connection only, to prevent race
	k.Lock()
	defer k.Unlock()
	if k.client != nil {
		return nil
	}

	if k.state.ProxyPid > 0 {
		// check that proxy is running before talk with it avoiding long timeouts
		if err := syscall.Kill(k.state.ProxyPid, syscall.Signal(0)); err != nil {
			return errors.New("Proxy is not running")
		}
	}

	k.Logger().WithField("url", k.state.URL).WithField("proxy", k.state.ProxyPid).Info("New client")
	client, err := kataclient.NewAgentClient(k.ctx, k.state.URL, k.proxyBuiltIn)
	if err != nil {
		k.dead = true
		return err
	}

	k.installReqFunc(client)
	k.client = client

	return nil
}

func (k *kataAgent) disconnect() error {
	span, _ := k.trace("disconnect")
	defer span.Finish()

	k.Lock()
	defer k.Unlock()

	if k.client == nil {
		return nil
	}

	if err := k.client.Close(); err != nil && grpcStatus.Convert(err).Code() != codes.Canceled {
		return err
	}

	k.client = nil
	k.reqHandlers = nil

	return nil
}

// check grpc server is serving
func (k *kataAgent) check() error {
	span, _ := k.trace("check")
	defer span.Finish()

	_, err := k.sendReq(&grpc.CheckRequest{})
	if err != nil {
		err = fmt.Errorf("Failed to check if grpc server is working: %s", err)
	}
	return err
}

func (k *kataAgent) waitProcess(c *Container, processID string) (int32, error) {
	span, _ := k.trace("waitProcess")
	defer span.Finish()

	resp, err := k.sendReq(&grpc.WaitProcessRequest{
		ContainerId: c.id,
		ExecId:      processID,
	})
	if err != nil {
		return 0, err
	}

	return resp.(*grpc.WaitProcessResponse).Status, nil
}

func (k *kataAgent) writeProcessStdin(c *Container, ProcessID string, data []byte) (int, error) {
	resp, err := k.sendReq(&grpc.WriteStreamRequest{
		ContainerId: c.id,
		ExecId:      ProcessID,
		Data:        data,
	})

	if err != nil {
		return 0, err
	}

	return int(resp.(*grpc.WriteStreamResponse).Len), nil
}

func (k *kataAgent) closeProcessStdin(c *Container, ProcessID string) error {
	_, err := k.sendReq(&grpc.CloseStdinRequest{
		ContainerId: c.id,
		ExecId:      ProcessID,
	})

	return err
}

func (k *kataAgent) reseedRNG(data []byte) error {
	_, err := k.sendReq(&grpc.ReseedRandomDevRequest{
		Data: data,
	})

	return err
}

type reqFunc func(context.Context, interface{}, ...golangGrpc.CallOption) (interface{}, error)

func (k *kataAgent) installReqFunc(c *kataclient.AgentClient) {
	k.reqHandlers = make(map[string]reqFunc)
	k.reqHandlers[grpcCheckRequest] = func(ctx context.Context, req interface{}, opts ...golangGrpc.CallOption) (interface{}, error) {
		return k.client.Check(ctx, req.(*grpc.CheckRequest), opts...)
	}
	k.reqHandlers[grpcExecProcessRequest] = func(ctx context.Context, req interface{}, opts ...golangGrpc.CallOption) (interface{}, error) {
		return k.client.ExecProcess(ctx, req.(*grpc.ExecProcessRequest), opts...)
	}
	k.reqHandlers[grpcCreateSandboxRequest] = func(ctx context.Context, req interface{}, opts ...golangGrpc.CallOption) (interface{}, error) {
		return k.client.CreateSandbox(ctx, req.(*grpc.CreateSandboxRequest), opts...)
	}
	k.reqHandlers[grpcDestroySandboxRequest] = func(ctx context.Context, req interface{}, opts ...golangGrpc.CallOption) (interface{}, error) {
		return k.client.DestroySandbox(ctx, req.(*grpc.DestroySandboxRequest), opts...)
	}
	k.reqHandlers[grpcCreateContainerRequest] = func(ctx context.Context, req interface{}, opts ...golangGrpc.CallOption) (interface{}, error) {
		return k.client.CreateContainer(ctx, req.(*grpc.CreateContainerRequest), opts...)
	}
	k.reqHandlers[grpcStartContainerRequest] = func(ctx context.Context, req interface{}, opts ...golangGrpc.CallOption) (interface{}, error) {
		return k.client.StartContainer(ctx, req.(*grpc.StartContainerRequest), opts...)
	}
	k.reqHandlers[grpcRemoveContainerRequest] = func(ctx context.Context, req interface{}, opts ...golangGrpc.CallOption) (interface{}, error) {
		return k.client.RemoveContainer(ctx, req.(*grpc.RemoveContainerRequest), opts...)
	}
	k.reqHandlers[grpcSignalProcessRequest] = func(ctx context.Context, req interface{}, opts ...golangGrpc.CallOption) (interface{}, error) {
		return k.client.SignalProcess(ctx, req.(*grpc.SignalProcessRequest), opts...)
	}
	k.reqHandlers[grpcUpdateRoutesRequest] = func(ctx context.Context, req interface{}, opts ...golangGrpc.CallOption) (interface{}, error) {
		return k.client.UpdateRoutes(ctx, req.(*grpc.UpdateRoutesRequest), opts...)
	}
	k.reqHandlers[grpcUpdateInterfaceRequest] = func(ctx context.Context, req interface{}, opts ...golangGrpc.CallOption) (interface{}, error) {
		return k.client.UpdateInterface(ctx, req.(*grpc.UpdateInterfaceRequest), opts...)
	}
	k.reqHandlers[grpcListInterfacesRequest] = func(ctx context.Context, req interface{}, opts ...golangGrpc.CallOption) (interface{}, error) {
		return k.client.ListInterfaces(ctx, req.(*grpc.ListInterfacesRequest), opts...)
	}
	k.reqHandlers[grpcListRoutesRequest] = func(ctx context.Context, req interface{}, opts ...golangGrpc.CallOption) (interface{}, error) {
		return k.client.ListRoutes(ctx, req.(*grpc.ListRoutesRequest), opts...)
	}
	k.reqHandlers[grpcAddARPNeighborsRequest] = func(ctx context.Context, req interface{}, opts ...golangGrpc.CallOption) (interface{}, error) {
		return k.client.AddARPNeighbors(ctx, req.(*grpc.AddARPNeighborsRequest), opts...)
	}
	k.reqHandlers[grpcOnlineCPUMemRequest] = func(ctx context.Context, req interface{}, opts ...golangGrpc.CallOption) (interface{}, error) {
		return k.client.OnlineCPUMem(ctx, req.(*grpc.OnlineCPUMemRequest), opts...)
	}
	k.reqHandlers[grpcListProcessesRequest] = func(ctx context.Context, req interface{}, opts ...golangGrpc.CallOption) (interface{}, error) {
		return k.client.ListProcesses(ctx, req.(*grpc.ListProcessesRequest), opts...)
	}
	k.reqHandlers[grpcUpdateContainerRequest] = func(ctx context.Context, req interface{}, opts ...golangGrpc.CallOption) (interface{}, error) {
		return k.client.UpdateContainer(ctx, req.(*grpc.UpdateContainerRequest), opts...)
	}
	k.reqHandlers[grpcWaitProcessRequest] = func(ctx context.Context, req interface{}, opts ...golangGrpc.CallOption) (interface{}, error) {
		return k.client.WaitProcess(ctx, req.(*grpc.WaitProcessRequest), opts...)
	}
	k.reqHandlers[grpcTtyWinResizeRequest] = func(ctx context.Context, req interface{}, opts ...golangGrpc.CallOption) (interface{}, error) {
		return k.client.TtyWinResize(ctx, req.(*grpc.TtyWinResizeRequest), opts...)
	}
	k.reqHandlers[grpcWriteStreamRequest] = func(ctx context.Context, req interface{}, opts ...golangGrpc.CallOption) (interface{}, error) {
		return k.client.WriteStdin(ctx, req.(*grpc.WriteStreamRequest), opts...)
	}
	k.reqHandlers[grpcCloseStdinRequest] = func(ctx context.Context, req interface{}, opts ...golangGrpc.CallOption) (interface{}, error) {
		return k.client.CloseStdin(ctx, req.(*grpc.CloseStdinRequest), opts...)
	}
	k.reqHandlers[grpcStatsContainerRequest] = func(ctx context.Context, req interface{}, opts ...golangGrpc.CallOption) (interface{}, error) {
		return k.client.StatsContainer(ctx, req.(*grpc.StatsContainerRequest), opts...)
	}
	k.reqHandlers[grpcPauseContainerRequest] = func(ctx context.Context, req interface{}, opts ...golangGrpc.CallOption) (interface{}, error) {
		return k.client.PauseContainer(ctx, req.(*grpc.PauseContainerRequest), opts...)
	}
	k.reqHandlers[grpcResumeContainerRequest] = func(ctx context.Context, req interface{}, opts ...golangGrpc.CallOption) (interface{}, error) {
		return k.client.ResumeContainer(ctx, req.(*grpc.ResumeContainerRequest), opts...)
	}
	k.reqHandlers[grpcReseedRandomDevRequest] = func(ctx context.Context, req interface{}, opts ...golangGrpc.CallOption) (interface{}, error) {
		return k.client.ReseedRandomDev(ctx, req.(*grpc.ReseedRandomDevRequest), opts...)
	}
	k.reqHandlers[grpcGuestDetailsRequest] = func(ctx context.Context, req interface{}, opts ...golangGrpc.CallOption) (interface{}, error) {
		return k.client.GetGuestDetails(ctx, req.(*grpc.GuestDetailsRequest), opts...)
	}
	k.reqHandlers[grpcMemHotplugByProbeRequest] = func(ctx context.Context, req interface{}, opts ...golangGrpc.CallOption) (interface{}, error) {
		return k.client.MemHotplugByProbe(ctx, req.(*grpc.MemHotplugByProbeRequest), opts...)
	}
	k.reqHandlers[grpcCopyFileRequest] = func(ctx context.Context, req interface{}, opts ...golangGrpc.CallOption) (interface{}, error) {
		return k.client.CopyFile(ctx, req.(*grpc.CopyFileRequest), opts...)
	}
	k.reqHandlers[grpcSetGuestDateTimeRequest] = func(ctx context.Context, req interface{}, opts ...golangGrpc.CallOption) (interface{}, error) {
		return k.client.SetGuestDateTime(ctx, req.(*grpc.SetGuestDateTimeRequest), opts...)
	}
	k.reqHandlers[grpcStartTracingRequest] = func(ctx context.Context, req interface{}, opts ...golangGrpc.CallOption) (interface{}, error) {
		return k.client.StartTracing(ctx, req.(*grpc.StartTracingRequest), opts...)
	}
	k.reqHandlers[grpcStopTracingRequest] = func(ctx context.Context, req interface{}, opts ...golangGrpc.CallOption) (interface{}, error) {
		return k.client.StopTracing(ctx, req.(*grpc.StopTracingRequest), opts...)
	}
	k.reqHandlers[grpcGetOOMEventRequest] = func(ctx context.Context, req interface{}, opts ...golangGrpc.CallOption) (interface{}, error) {
		return k.client.GetOOMEvent(ctx, req.(*grpc.GetOOMEventRequest), opts...)
	}
}

func (k *kataAgent) getReqContext(reqName string) (ctx context.Context, cancel context.CancelFunc) {
	ctx = context.Background()
	switch reqName {
	case grpcWaitProcessRequest, grpcGetOOMEventRequest:
		// Wait and GetOOMEvent have no timeout
	case grpcCheckRequest:
		ctx, cancel = context.WithTimeout(ctx, checkRequestTimeout)
	default:
		ctx, cancel = context.WithTimeout(ctx, defaultRequestTimeout)
	}

	return ctx, cancel
}

func (k *kataAgent) sendReq(request interface{}) (interface{}, error) {
	span, _ := k.trace("sendReq")
	span.SetTag("request", request)
	defer span.Finish()

	if err := k.connect(); err != nil {
		return nil, err
	}
	if !k.keepConn {
		defer k.disconnect()
	}

	msgName := proto.MessageName(request.(proto.Message))
	handler := k.reqHandlers[msgName]
	if msgName == "" || handler == nil {
		return nil, errors.New("Invalid request type")
	}
	message := request.(proto.Message)
	ctx, cancel := k.getReqContext(msgName)
	if cancel != nil {
		defer cancel()
	}
	k.Logger().WithField("name", msgName).WithField("req", message.String()).Debug("sending request")

	return handler(ctx, request)
}

// readStdout and readStderr are special that we cannot differentiate them with the request types...
func (k *kataAgent) readProcessStdout(c *Container, processID string, data []byte) (int, error) {
	if err := k.connect(); err != nil {
		return 0, err
	}
	if !k.keepConn {
		defer k.disconnect()
	}

	return k.readProcessStream(c.id, processID, data, k.client.ReadStdout)
}

// readStdout and readStderr are special that we cannot differentiate them with the request types...
func (k *kataAgent) readProcessStderr(c *Container, processID string, data []byte) (int, error) {
	if err := k.connect(); err != nil {
		return 0, err
	}
	if !k.keepConn {
		defer k.disconnect()
	}

	return k.readProcessStream(c.id, processID, data, k.client.ReadStderr)
}

type readFn func(context.Context, *grpc.ReadStreamRequest, ...golangGrpc.CallOption) (*grpc.ReadStreamResponse, error)

func (k *kataAgent) readProcessStream(containerID, processID string, data []byte, read readFn) (int, error) {
	resp, err := read(k.ctx, &grpc.ReadStreamRequest{
		ContainerId: containerID,
		ExecId:      processID,
		Len:         uint32(len(data))})
	if err == nil {
		copy(data, resp.Data)
		return len(resp.Data), nil
	}

	return 0, err
}

func (k *kataAgent) getGuestDetails(req *grpc.GuestDetailsRequest) (*grpc.GuestDetailsResponse, error) {
	resp, err := k.sendReq(req)
	if err != nil {
		return nil, err
	}

	return resp.(*grpc.GuestDetailsResponse), nil
}

func (k *kataAgent) setGuestDateTime(tv time.Time) error {
	_, err := k.sendReq(&grpc.SetGuestDateTimeRequest{
		Sec:  tv.Unix(),
		Usec: int64(tv.Nanosecond() / 1e3),
	})

	return err
}

func (k *kataAgent) convertToKataAgentIPFamily(ipFamily int) aTypes.IPFamily {
	switch ipFamily {
	case netlink.FAMILY_V4:
		return aTypes.IPFamily_v4
	case netlink.FAMILY_V6:
		return aTypes.IPFamily_v6
	}

	return aTypes.IPFamily_v4
}

func (k *kataAgent) convertToIPFamily(ipFamily aTypes.IPFamily) int {
	switch ipFamily {
	case aTypes.IPFamily_v4:
		return netlink.FAMILY_V4
	case aTypes.IPFamily_v6:
		return netlink.FAMILY_V6
	}

	return netlink.FAMILY_V4
}

func (k *kataAgent) convertToKataAgentIPAddress(ipAddr *vcTypes.IPAddress) (aIPAddr *aTypes.IPAddress) {
	if ipAddr == nil {
		return nil
	}

	aIPAddr = &aTypes.IPAddress{
		Family:  k.convertToKataAgentIPFamily(ipAddr.Family),
		Address: ipAddr.Address,
		Mask:    ipAddr.Mask,
	}

	return aIPAddr
}

func (k *kataAgent) convertToKataAgentIPAddresses(ipAddrs []*vcTypes.IPAddress) (aIPAddrs []*aTypes.IPAddress) {
	for _, ipAddr := range ipAddrs {
		if ipAddr == nil {
			continue
		}

		aIPAddr := k.convertToKataAgentIPAddress(ipAddr)
		aIPAddrs = append(aIPAddrs, aIPAddr)
	}

	return aIPAddrs
}

func (k *kataAgent) convertToIPAddresses(aIPAddrs []*aTypes.IPAddress) (ipAddrs []*vcTypes.IPAddress) {
	for _, aIPAddr := range aIPAddrs {
		if aIPAddr == nil {
			continue
		}

		ipAddr := &vcTypes.IPAddress{
			Family:  k.convertToIPFamily(aIPAddr.Family),
			Address: aIPAddr.Address,
			Mask:    aIPAddr.Mask,
		}

		ipAddrs = append(ipAddrs, ipAddr)
	}

	return ipAddrs
}

func (k *kataAgent) convertToKataAgentInterface(iface *vcTypes.Interface) *aTypes.Interface {
	if iface == nil {
		return nil
	}

	return &aTypes.Interface{
		Device:      iface.Device,
		Name:        iface.Name,
		IPAddresses: k.convertToKataAgentIPAddresses(iface.IPAddresses),
		Mtu:         iface.Mtu,
		RawFlags:    iface.RawFlags,
		HwAddr:      iface.HwAddr,
		PciPath:     iface.PciPath.String(),
	}
}

func (k *kataAgent) convertToInterfaces(aIfaces []*aTypes.Interface) ([]*vcTypes.Interface, error) {
	ifaces := make([]*vcTypes.Interface, 0)
	for _, aIface := range aIfaces {
		if aIface == nil {
			continue
		}

		pcipath, err := vcTypes.PciPathFromString(aIface.PciPath)
		if err != nil {
			return nil, err
		}
		iface := &vcTypes.Interface{
			Device:      aIface.Device,
			Name:        aIface.Name,
			IPAddresses: k.convertToIPAddresses(aIface.IPAddresses),
			Mtu:         aIface.Mtu,
			HwAddr:      aIface.HwAddr,
			PciPath:     pcipath,
		}

		ifaces = append(ifaces, iface)
	}

	return ifaces, nil
}

func (k *kataAgent) convertToKataAgentRoutes(routes []*vcTypes.Route) (aRoutes []*aTypes.Route) {
	for _, route := range routes {
		if route == nil {
			continue
		}

		aRoute := &aTypes.Route{
			Dest:    route.Dest,
			Gateway: route.Gateway,
			Device:  route.Device,
			Source:  route.Source,
			Scope:   route.Scope,
		}

		aRoutes = append(aRoutes, aRoute)
	}

	return aRoutes
}

func (k *kataAgent) convertToKataAgentNeighbors(neighs []*vcTypes.ARPNeighbor) (aNeighs []*aTypes.ARPNeighbor) {
	for _, neigh := range neighs {
		if neigh == nil {
			continue
		}

		aNeigh := &aTypes.ARPNeighbor{
			ToIPAddress: k.convertToKataAgentIPAddress(neigh.ToIPAddress),
			Device:      neigh.Device,
			State:       int32(neigh.State),
			Lladdr:      neigh.LLAddr,
		}

		aNeighs = append(aNeighs, aNeigh)
	}

	return aNeighs
}

func (k *kataAgent) convertToRoutes(aRoutes []*aTypes.Route) (routes []*vcTypes.Route) {
	for _, aRoute := range aRoutes {
		if aRoute == nil {
			continue
		}

		route := &vcTypes.Route{
			Dest:    aRoute.Dest,
			Gateway: aRoute.Gateway,
			Device:  aRoute.Device,
			Source:  aRoute.Source,
			Scope:   aRoute.Scope,
		}

		routes = append(routes, route)
	}

	return routes
}

func (k *kataAgent) copyFile(src, dst string) error {
	var st unix.Stat_t

	err := unix.Stat(src, &st)
	if err != nil {
		return fmt.Errorf("Could not get file %s information: %v", src, err)
	}

	b, err := ioutil.ReadFile(src)
	if err != nil {
		return fmt.Errorf("Could not read file %s: %v", src, err)
	}

	fileSize := int64(len(b))

	k.Logger().WithFields(logrus.Fields{
		"source": src,
		"dest":   dst,
	}).Debugf("Copying file from host to guest")

	cpReq := &grpc.CopyFileRequest{
		Path:     dst,
		DirMode:  uint32(DirMode),
		FileMode: st.Mode,
		FileSize: fileSize,
		Uid:      int32(st.Uid),
		Gid:      int32(st.Gid),
	}

	// Handle the special case where the file is empty
	if fileSize == 0 {
		_, err = k.sendReq(cpReq)
		return err
	}

	// Copy file by parts if it's needed
	remainingBytes := fileSize
	offset := int64(0)
	for remainingBytes > 0 {
		bytesToCopy := int64(len(b))
		if bytesToCopy > grpcMaxDataSize {
			bytesToCopy = grpcMaxDataSize
		}

		cpReq.Data = b[:bytesToCopy]
		cpReq.Offset = offset

		if _, err = k.sendReq(cpReq); err != nil {
			return fmt.Errorf("Could not send CopyFile request: %v", err)
		}

		b = b[bytesToCopy:]
		remainingBytes -= bytesToCopy
		offset += grpcMaxDataSize
	}

	return nil
}

func (k *kataAgent) markDead() {
	k.Logger().Infof("mark agent dead")
	k.dead = true
	k.disconnect()
}

func (k *kataAgent) cleanup(s *Sandbox) {
	// Unmount shared path
	path := getSharePath(s.id)
	k.Logger().WithField("path", path).Infof("cleanup agent")
	if err := syscall.Unmount(path, syscall.MNT_DETACH|UmountNoFollow); err != nil {
		k.Logger().WithError(err).Errorf("failed to unmount vm share path %s", path)
	}

	// Unmount mount path
	path = getMountPath(s.id)
	if err := bindUnmountAllRootfs(k.ctx, path, s); err != nil {
		k.Logger().WithError(err).Errorf("failed to unmount vm mount path %s", path)
	}
	if err := os.RemoveAll(getSandboxPath(s.id)); err != nil {
		k.Logger().WithError(err).Errorf("failed to cleanup vm path %s", getSandboxPath(s.id))
	}
}

func (k *kataAgent) save() persistapi.AgentState {
	return persistapi.AgentState{
		ProxyPid: k.state.ProxyPid,
		URL:      k.state.URL,
	}
}

func (k *kataAgent) load(s persistapi.AgentState) {
	k.state.ProxyPid = s.ProxyPid
	k.state.URL = s.URL
}

func (k *kataAgent) getOOMEvent() (string, error) {
	req := &grpc.GetOOMEventRequest{}
	result, err := k.sendReq(req)
	if err != nil {
		return "", err
	}
	if oomEvent, ok := result.(*grpc.OOMEvent); ok {
		return oomEvent.ContainerId, nil
	}
	return "", err
}
