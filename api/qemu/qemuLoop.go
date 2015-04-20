package qemu

import (
    "os/exec"
    "net"
    "dvm/api/pod"
    "dvm/api/network"
    "dvm/api/types"
    "dvm/lib/glog"
    "encoding/json"
    "io"
    "strings"
    "fmt"
    "time"
)

// Event messages for chan-ctrl

type QemuEvent interface {
    Event() int
}

type QemuExitEvent struct {
    message string
}

type QemuTimeout struct {}

type InitFailedEvent struct {
    reason string
}

type InitConnectedEvent struct {
    conn *net.UnixConn
}

type RunPodCommand struct {
    Spec *pod.UserPod
}

type ExecCommand struct {
    Command []string `json:"cmd"`
    Container string `json:"container,omitempty"`
}

type ShutdownCommand struct {}

type AttachCommand struct {
    container string
    callback  chan *TtyIO
}

type DetachCommand struct{
    container string
    tty       *TtyIO
}

type CommandAck struct {
    reply   uint32
    msg     []byte
}

type ContainerCreatedEvent struct {
    Index   int
    Id      string
    Rootfs  string
    Image   string          // if fstype is `dir`, this should be a path relative to share_dir
                            // which described the mounted aufs or overlayfs dir.
    Fstype  string
    Workdir string
    Cmd     []string
    Envs    map[string]string
}

type VolumeReadyEvent struct {
    Name        string      //volumen name in spec
    Filepath    string      //block dev absolute path, or dir path relative to share dir
    Fstype      string      //"xfs", "ext4" etc. for block dev, or "dir" for dir path
    Format      string      //"raw" (or "qcow2") for volume, no meaning for dir path
}

type BlockdevInsertedEvent struct {
    Name        string
    SourceType  string //image or volume
    DeviceName  string
}

type InterfaceCreated struct {
    Index       int
    PCIAddr     int
    Fd          uint64
    DeviceName  string
    IpAddr      string
    NetMask     string
    RouteTable  []*RouteRule
}

type RouteRule struct {
    Destination string
    Gateway     string
    ViaThis     bool
}

type NetDevInsertedEvent struct {
    Index       int
    DeviceName  string
    Address     int
}

type SerialAddEvent struct {
    Index       int
    PortName    string
}

type TtyOpenEvent struct {
    Index       int
    TC          *ttyContext
}

func (qe* QemuExitEvent)            Event() int { return EVENT_QEMU_EXIT }
func (qe* QemuTimeout)              Event() int { return EVENT_QEMU_TIMEOUT }
func (qe* InitConnectedEvent)       Event() int { return EVENT_INIT_CONNECTED }
func (qe* RunPodCommand)            Event() int { return COMMAND_RUN_POD }
func (qe* ExecCommand)              Event() int { return COMMAND_EXEC }
func (qe* AttachCommand)            Event() int { return COMMAND_ATTACH }
func (qe* DetachCommand)            Event() int { return COMMAND_DETACH }
func (qe* ContainerCreatedEvent)    Event() int { return EVENT_CONTAINER_ADD }
func (qe* VolumeReadyEvent)         Event() int { return EVENT_VOLUME_ADD }
func (qe* BlockdevInsertedEvent)    Event() int { return EVENT_BLOCK_INSERTED }
func (qe* CommandAck)               Event() int { return COMMAND_ACK }
func (qe* InterfaceCreated)         Event() int { return EVENT_INTERFACE_ADD }
func (qe* NetDevInsertedEvent)      Event() int { return EVENT_INTERFACE_INSERTED }
func (qe* ShutdownCommand)          Event() int { return COMMAND_SHUTDOWN }
func (qe* InitFailedEvent)          Event() int { return ERROR_INIT_FAIL }
func (qe* TtyOpenEvent)             Event() int { return EVENT_TTY_OPEN }
func (qe* SerialAddEvent)           Event() int { return EVENT_SERIAL_ADD }

// routines:

func CreateInterface(index int, pciAddr int, name string, isDefault bool, callback chan QemuEvent) {
    inf, err := network.Allocate("")
    if err != nil {
        glog.Error("interface creating failed", err.Error())
        callback <- &InterfaceCreated{
            Index:      index,
            PCIAddr:    pciAddr,
            DeviceName: name,
            Fd:         0,
            IpAddr:     "",
            NetMask:    "",
            RouteTable: nil,
        }
        return
    }

    interfaceGot(index, pciAddr, name, isDefault, callback, inf)
}

func interfaceGot(index int, pciAddr int, name string, isDefault bool, callback chan QemuEvent, inf *network.Settings) {

    ip,nw,err := net.ParseCIDR(fmt.Sprintf("%s/%d", inf.IPAddress, inf.IPPrefixLen))
    if err != nil {
        glog.Error("can not parse cidr")
        callback <- &InterfaceCreated{
            Index:      index,
            PCIAddr:    pciAddr,
            DeviceName: name,
            Fd:         0,
            IpAddr:     "",
            NetMask:    "",
            RouteTable: nil,
        }
        return
    }
    var tmp []byte = nw.Mask
    var mask net.IP = tmp

    rt:=[]*RouteRule{
//        &RouteRule{
//            Destination: fmt.Sprintf("%s/%d", nw.IP.String(), inf.IPPrefixLen),
//            Gateway:"", ViaThis:true,
//        },
    }
    if isDefault {
        rt = append(rt, &RouteRule{
            Destination: "0.0.0.0/0",
            Gateway: inf.Gateway, ViaThis: true,
        })
    }

    event := &InterfaceCreated{
        Index:      index,
        PCIAddr:    pciAddr,
        DeviceName: name,
        Fd:         uint64(inf.File.Fd()),
        IpAddr:     ip.String(),
        NetMask:    mask.String(),
        RouteTable: rt,
    }

    callback <- event
}

func printDebugOutput(tag string, out io.ReadCloser) {
    buf := make([]byte, 1024)
    for {
        n,err:=out.Read(buf)
        if err == io.EOF {
            glog.V(0).Infof("%s finish", tag)
            break
        } else if err != nil {
            glog.Error(err)
        }
        glog.V(0).Infof("got %s: %s", tag, string(buf[:n]))
    }
}

func waitConsoleOutput(ctx *QemuContext) {
    conn, err := ctx.consoleSock.AcceptUnix()
    if err != nil {
        glog.Warning(err.Error())
        return
    }

    tc := setupTty(ctx.consoleSockName, conn, make(chan interface{}))
    tty := tc.Get()
    ctx.consoleTty = tc
    tc.start()

    for {
        line,ok := <- tty.Output
        if ok {
            glog.V(1).Info("[console] ", line)
        } else {
            glog.Info("console output end")
            break
        }
    }
}

// launchQemu run qemu and wait it's quit, includes
func launchQemu(ctx *QemuContext) {
    qemu,err := exec.LookPath("qemu-system-x86_64")
    if  err != nil {
        ctx.hub <- &QemuExitEvent{message:"can not find qemu executable"}
        return
    }

    args := ctx.QemuArguments()

    glog.V(0).Info("cmdline arguments: ", strings.Join(args, " "))

    cmd := exec.Command(qemu, args...)

    stderr,err := cmd.StderrPipe()
    if err != nil {
        glog.Warning("Cannot get stderr of qemu")
    }

    go printDebugOutput("stderr", stderr)

    if err := cmd.Start();err != nil {
        glog.Error("try to start qemu failed")
        ctx.hub <- &QemuExitEvent{message:"try to start qemu failed"}
        return
    }

    glog.V(0).Info("Waiting for command to finish...")

    err = cmd.Wait()
    if err != nil {
        glog.Info("qemu exit with ", err.Error())
        ctx.hub <- &QemuExitEvent{message:"qemu exit with " + err.Error()}
    } else {
        glog.Info("qemu exit with 0")
        ctx.hub <- &QemuExitEvent{message:"qemu exit with 0"}
    }
}

func onQemuExit(ctx *QemuContext) {
    ctx.Become(stateCleaningUp)
}

func prepareDevice(ctx *QemuContext, spec *pod.UserPod) {
    networks := 1
    ctx.InitDeviceContext(spec, networks)
    res,_ := json.MarshalIndent(*ctx.vmSpec, "    ", "    ")
    glog.V(2).Info("initial vm spec: ",string(res))
    go CreateContainer(spec, ctx.shareDir, ctx.hub)
    if networks > 0 {
        for i:=0; i < networks; i++ {
            name := fmt.Sprintf("eth%d", i)
            addr := ctx.nextPciAddr()
            go CreateInterface(i, addr, name, i == 0, ctx.hub)
        }
    }
    for i:=0; i < len(ctx.userSpec.Containers); i++ {
        go attachSerialPort(ctx, i)
    }
    for blk,_ := range ctx.progress.adding.blockdevs {
        info := ctx.devices.volumeMap[blk]
        sid := ctx.nextScsiId()
        ctx.qmp <- newDiskAddSession(ctx, info.info.name, "volume", info.info.filename, info.info.format, sid)
    }
}

func runPod(ctx *QemuContext) {
    pod,err := json.Marshal(*ctx.vmSpec)
    if err != nil {
        //TODO: fail exit
        return
    }
    ctx.vm <- &DecodedMessage{
        code: INIT_STARTPOD,
        message: pod,
    }
}

// state machine
func commonStateHandler(ctx *QemuContext, ev QemuEvent) bool {
    switch ev.Event() {
    case EVENT_QEMU_EXIT:
        glog.Info("Qemu has exit, go to cleaning up")
        onQemuExit(ctx)
        ctx.Close()
        return true
    case EVENT_QMP_EVENT:
        event := ev.(*QmpEvent)
        if event.Type == QMP_EVENT_SHUTDOWN {
            glog.Info("Got QMP shutdown event, go to cleaning up")
            onQemuExit(ctx)
            return true
        }
        return false
    case COMMAND_SHUTDOWN:
        ctx.vm <- &DecodedMessage{ code: INIT_SHUTDOWN, message: []byte{}, }
        time.AfterFunc(3*time.Second, func(){
            if ctx.handler != nil {
                ctx.hub <- &QemuTimeout{}
            }
        })
        glog.Info("shutdown command sent, now get into terminating state")
        ctx.Become(stateTerminating)
        return true
    default:
        return false
    }
}

func stateInit(ctx *QemuContext, ev QemuEvent) {
    if processed := commonStateHandler(ctx, ev); !processed {
        switch ev.Event() {
            case EVENT_INIT_CONNECTED:
                event := ev.(*InitConnectedEvent)
                if event.conn != nil {
                    glog.Info("begin to wait dvm commands")
                    go waitCmdToInit(ctx, event.conn)
                } else {
                    // TODO: fail exit
                }
            case COMMAND_RUN_POD:
                glog.Info("got spec, prepare devices")
                prepareDevice(ctx, ev.(*RunPodCommand).Spec)
            case COMMAND_ACK:
                ack := ev.(*CommandAck)
                if ack.reply == INIT_STARTPOD {
                    glog.Info("run success", string(ack.msg))
                    ctx.client <- &types.QemuResponse{
                        VmId: ctx.id,
                        Code: types.E_OK,
                        Cause: "Start POD success",
                    }
                    ctx.Become(stateRunning)
                } else {
                    glog.Warning("wrong reply to ", string(ack.reply), string(ack.msg))
                }
            case EVENT_CONTAINER_ADD:
                info := ev.(*ContainerCreatedEvent)
                needInsert := ctx.containerCreated(info)
                if needInsert {
                    sid := ctx.nextScsiId()
                    ctx.qmp <- newDiskAddSession(ctx, info.Image, "image", info.Image, "raw", sid)
                } else if ctx.deviceReady() {
                    glog.V(1).Info("device ready, could run pod.")
                    runPod(ctx)
                }
            case EVENT_VOLUME_ADD:
                info := ev.(*VolumeReadyEvent)
                needInsert := ctx.volumeReady(info)
                if needInsert {
                    sid := ctx.nextScsiId()
                    ctx.qmp <- newDiskAddSession(ctx, info.Name, "volume", info.Filepath, info.Format, sid)
                } else if ctx.deviceReady() {
                    glog.V(1).Info("device ready, could run pod.")
                    runPod(ctx)
                }
            case EVENT_BLOCK_INSERTED:
                info := ev.(*BlockdevInsertedEvent)
                ctx.blockdevInserted(info)
                if ctx.deviceReady() {
                    glog.V(1).Info("device ready, could run pod.")
                    runPod(ctx)
                }
            case EVENT_INTERFACE_ADD:
                info := ev.(*InterfaceCreated)
                if info.IpAddr != "" {
                    ctx.interfaceCreated(info)
                    ctx.qmp <- newNetworkAddSession(ctx, info.Fd, info.DeviceName, info.Index, info.PCIAddr)
                } else {
                    ctx.client <- &types.QemuResponse{
                        VmId: ctx.id,
                        Code: types.E_DEVICE_FAIL,
                        Cause: fmt.Sprintf("network interface %d creation fail", info.Index),
                    }
                }
            case EVENT_INTERFACE_INSERTED:
                info := ev.(*NetDevInsertedEvent)
                ctx.netdevInserted(info)
                if ctx.deviceReady() {
                    glog.V(1).Info("device ready, could run pod.")
                    runPod(ctx)
                }
            case EVENT_SERIAL_ADD:
                info := ev.(*SerialAddEvent)
                ctx.serialAttached(info)
                if ctx.deviceReady() {
                    glog.V(1).Info("device ready, could run pod.")
                    runPod(ctx)
                }
            case EVENT_TTY_OPEN:
                info := ev.(*TtyOpenEvent)
                ctx.ttyOpened(info)
                if ctx.deviceReady() {
                    glog.V(1).Info("device ready, could run pod.")
                    runPod(ctx)
                }
            case ERROR_INIT_FAIL:
                reason := ev.(*InitFailedEvent).reason
                ctx.client <- &types.QemuResponse{
                    VmId: ctx.id,
                    Code: types.E_INIT_FAIL,
                    Cause: reason,
                }
            default:
                glog.Warning("got event during pod initiating")
        }
    }
}

func stateRunning(ctx *QemuContext, ev QemuEvent) {
    if processed := commonStateHandler(ctx, ev); !processed {
        switch ev.Event() {
            case COMMAND_EXEC:
            cmd := ev.(*ExecCommand)
            pkg,err := json.Marshal(*cmd)
            if err != nil {
                ctx.client <- &types.QemuResponse{
                    VmId: ctx.id,
                    Code: types.E_JSON_PARSE_FAIL,
                    Cause: fmt.Sprintf("command %s parse failed", cmd.Command,),
                }
                return
            }
            ctx.vm <- &DecodedMessage{
                code: INIT_EXECCMD,
                message: pkg,
            }
            case COMMAND_ACK:
            ack := ev.(*CommandAck)
            if ack.reply == INIT_EXECCMD {
                glog.Info("exec dvm run confirmed", string(ack.msg))
            } else {
                glog.Warning("[Running] wrong reply to ", string(ack.reply), string(ack.msg))
            }
            case COMMAND_ATTACH:
                cmd := ev.(*AttachCommand)
                if cmd.container == "" { //console
                    glog.V(1).Info("Allocating vm console tty.")
                    cmd.callback <- ctx.consoleTty.Get()
                } else if idx := ctx.Lookup( cmd.container ); idx >= 0 {
                    glog.V(1).Info("Allocating tty for ", cmd.container)
                    tc := ctx.devices.ttyMap[idx]
                    cmd.callback <- tc.Get()
                }
            case COMMAND_DETACH:
                cmd := ev.(*DetachCommand)
                if cmd.container == "" {
                    glog.V(1).Info("Drop vm console tty.")
                    ctx.consoleTty.Drop(cmd.tty)
                } else if idx := ctx.Lookup( cmd.container ); idx >= 0 {
                    glog.V(1).Info("Drop tty for ", cmd.container)
                    tc := ctx.devices.ttyMap[idx]
                    tc.Drop(cmd.tty)
                }
            default:
                glog.Warning("got event during pod running")
        }
    }
}

func stateTerminating(ctx *QemuContext, ev QemuEvent) {
    if processed := commonStateHandler(ctx, ev); !processed {
        switch ev.Event() {
            case COMMAND_ACK:
                ack := ev.(*CommandAck)
                if ack.reply == INIT_SHUTDOWN {
                    glog.Info("Shutting down command was accepted by init", string(ack.msg))
                } else {
                    glog.Warning("[Terminating] wrong reply to ", string(ack.reply), string(ack.msg))
                }
            case EVENT_QEMU_TIMEOUT:
                glog.Warning("Qemu did not exit in time, try to stop it")
                ctx.qmp <- newQuitSession()
        }
    }
}

func stateCleaningUp(ctx *QemuContext, ev QemuEvent) {
    switch ev.Event() {
        case EVENT_QEMU_EXIT:
            glog.Info("Qemu has exit [cleaning up]")
            ctx.Close()
            ctx.Become(nil)
            ctx.client <- &types.QemuResponse{
                VmId: ctx.id,
                Code: types.E_SHUTDOWM,
                Cause: "qemu shut down",
            }
        default:
            glog.Warning("got event during pod cleaning up")
    }
}

// main loop

func QemuLoop(dvmId string, hub chan QemuEvent, client chan *types.QemuResponse, cpu, memory int) {
    context,err := initContext(dvmId, hub, client, cpu, memory)
    if err != nil {
        client <- &types.QemuResponse{
            VmId: dvmId,
            Code: types.E_CONTEXT_INIT_FAIL,
            Cause: err.Error(),
        }
        return
    }

    //launch routines
    go qmpHandler(context)
    go waitInitReady(context)
    go launchQemu(context)
    go waitConsoleOutput(context)

    for context != nil && context.handler != nil {
        ev,ok := <-context.hub
        if !ok {
            glog.Error("hub chan has already been closed")
            break
        }
        glog.V(1).Infof("main event loop got message %d(%s)", ev.Event(), EventString(ev.Event()))
        context.handler(context, ev)
    }
}

//func main() {
//    qemuChan := make(chan QemuEvent, 128)
//    go qemuLoop("mydvm", qemuChan, 1, 128)
//    //qemuChan <- podSpec
//    for {
//    }
//}
