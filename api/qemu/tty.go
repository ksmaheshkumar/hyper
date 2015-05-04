package qemu

import (
    "io"
    "net"
    "dvm/lib/glog"
    "dvm/lib/telnet"
    "strconv"
    "os"
    "time"
    "sync"
    "errors"
    "encoding/binary"
    "dvm/api/types"
)

type WindowSize struct {
    Row         uint16 `json:"row"`
    Column      uint16 `json:"column"`
}

type TtyIO struct {
    Stdin   io.ReadCloser
    Stdout  io.WriteCloser
    ClientTag string
    Callback chan *types.QemuResponse
}

type ttyContext struct {
    socketName  string
    vmConn      net.Conn
    subscribers map[uint64]*TtyIO
    lock        *sync.Mutex
}

type ttyAttachments struct {
    container   int
    persistent  bool
    attachments []*TtyIO
}

type pseudoTtys struct {
    channel     chan *ttyMessage
    ttys        map[uint64]*ttyAttachments
    lock        *sync.Mutex
}

type ttyMessage struct {
    session     uint64
    message     []byte
}

func newPts() *pseudoTtys {
    return &pseudoTtys{
        channel: make(chan *ttyMessage, 256),
        ttys: make(map[uint64]*ttyAttachments),
        lock: &sync.Mutex{},
    }
}

func readTtyMessage(conn *net.UnixConn) (*ttyMessage, error) {
    needRead := 12
    length   := 0
    read     :=0
    buf := make([]byte, 512)
    res := []byte{}
    for read < needRead {
        want := needRead - read
        if want > 512 {
            want = 512
        }
        glog.V(1).Infof("tty: trying to read %d bytes", want)
        nr,err := conn.Read(buf[:want])
        if err != nil {
            glog.Error("read tty data failed", )
            return nil, err
        }

        res = append(res, buf[:nr]...)
        read = read + nr

        glog.V(1).Infof("tty: read %d/%d [length = %d]", read, needRead, length)

        if length == 0 && read >= 12 {
            length = int(binary.BigEndian.Uint32(res[8:12]))
            glog.V(1).Infof("data length is %d", length)
            if length > 12 {
                needRead = length
            }
        }
    }

    return &ttyMessage{
        session: binary.BigEndian.Uint64(res[:8]),
        message: res[12:],
    },nil
}

func waitTtyMessage(ctx *QemuContext, conn *net.UnixConn) {
    for {
        msg,ok := <-ctx.ptys.channel
        if !ok {
            glog.V(1).Info("tty chan closed, quit sent goroutine")
            break
        }
        if _,ok := ctx.ptys.ttys[msg.session]; ok {
            _,err := conn.Write(msg.message)
            if err != nil {
                glog.V(1).Info("Cannot write to tty socket: ", err.Error())
                return
            }
        }
    }
}

func waitPts(ctx *QemuContext) {
    conn, err := ctx.ttySock.AcceptUnix()
    if err != nil {
        glog.Error("Cannot accept tty socket ", err.Error())
        ctx.hub <- &InitFailedEvent{
            reason: "Cannot accept tty socket " + err.Error(),
        }
        return
    }

    go waitTtyMessage(ctx, conn)

    for {
        res,err := readTtyMessage(conn)
        if err != nil {
            glog.V(1).Info("tty socket closed, quit the reading goroutine ", err.Error())
            ctx.hub <- &Interrupted{ reason: "tty socket failed " + err.Error(), }
            close(ctx.ptys.channel)
            return
        }
        if ta,ok := ctx.ptys.ttys[res.session]; ok {
            if len(res.message) == 0 {
                glog.V(1).Infof("session %d closed by peer, close pty", res.session)
                ctx.ptys.Close(ctx, res.session)
            } else{
                for _,tty := range ta.attachments {
                    if tty.Stdout != nil {
                        _,err := tty.Stdout.Write(res.message)
                        if err != nil {
                            glog.V(1).Infof("fail to write session %d, close pty attachment", res.session)
                            ctx.ptys.Detach(ctx, res.session, tty)
                        }
                    }
                }
            }
        }
    }
}

func newAttachments(idx int, persist bool) *ttyAttachments {
    return &ttyAttachments{
        container: idx,
        persistent: persist,
        attachments: []*TtyIO{},
    }
}


func newAttachmentsWithTty(idx int, persist bool, tty *TtyIO) *ttyAttachments {
    return &ttyAttachments{
        container: idx,
        persistent: persist,
        attachments: []*TtyIO{tty,},
    }
}

func (ta *ttyAttachments) attach(tty *TtyIO) {
    ta.attachments = append(ta.attachments, tty)
}

func (ta *ttyAttachments) detach(tty *TtyIO) {
    at := []*TtyIO{}
    detached := false
    for _,t := range ta.attachments {
        if tty.ClientTag != t.ClientTag {
            at = append(at, t)
        } else {
            detached = true
        }
    }
    if detached {
        ta.attachments = at
    }
}

func (ta *ttyAttachments) close() []string {
    tags := []string{}
    for _,t := range ta.attachments {
        tags = append(tags, t.Close())
    }
    ta.attachments = []*TtyIO{}
    return tags
}

func (ta *ttyAttachments) empty() bool {
    return len(ta.attachments) == 0
}

func (tty *TtyIO) Close() string {
    if tty.Stdin != nil {
        tty.Stdin.Close()
    }
    if tty.Stdout != nil {
        tty.Stdout.Close()
    }
    if tty.Callback != nil {
        tty.Callback <- &types.QemuResponse{
            Code:  types.E_EXEC_FINISH,
            Cause: "Command finished",
        }
    }
    return tty.ClientTag
}

func (pts *pseudoTtys) Detach(ctx*QemuContext, session uint64, tty *TtyIO) {
    if ta,ok := ctx.ptys.ttys[session] ; ok {
        ctx.ptys.lock.Lock()
        ta.detach(tty)
        ctx.ptys.lock.Unlock()
        if !ta.persistent && ta.empty() {
            ctx.ptys.Close(ctx, session)
        }
        ctx.clientDereg(tty.Close())
    }
}

func (pts *pseudoTtys) Close(ctx *QemuContext, session uint64) {
    if ta,ok := pts.ttys[session]; ok {
        pts.lock.Lock()
        tags := ta.close()
        delete(pts.ttys, session)
        pts.lock.Unlock()
        for _,t := range tags {
            ctx.clientDereg(t)
        }
    }
}

func (pts *pseudoTtys) ptyConnect(ctx *QemuContext, container int, session uint64, tty *TtyIO) {

    pts.lock.Lock()
    if ta,ok := pts.ttys[session]; ok {
        ta.attach(tty)
    } else {
        pts.ttys[session] = newAttachmentsWithTty(container, false, tty)
    }
    pts.lock.Unlock()

    if tty.Stdin != nil {
        go func() {
            buf := make([]byte, 1)
            defer pts.Detach(ctx, session, tty)
            for {
                nr,err := tty.Stdin.Read(buf)
                if err != nil {
                    glog.Info("a stdin closed, ", err.Error())
                    return
                } else if nr == 1 && buf[0] == ExitChar {
                    glog.Info("got stdin detach char, exit term")
                    return
                }

                pts.channel <- &ttyMessage{
                    session: session,
                    message: buf[:nr],
                }
            }
        }()
    }

    return
}

func setupTty(ctx *QemuContext, name string, conn *net.UnixConn, tn bool, initIO *TtyIO) *ttyContext {
    var c net.Conn = conn
    if tn == true {
        tc,err := telnet.NewConn(conn)
        if err != nil {
            glog.Error("fail to init telnet connection to ", name, ": ", err.Error())
            return nil
        }
        glog.V(1).Infof("connected %s as telnet mode.", name)
        c = tc
    }

    ttyc := &ttyContext{
        socketName: name,
        vmConn:     c,
        subscribers:make(map[uint64]*TtyIO),
        lock:       &sync.Mutex{},
    }

    ttyc.connect(ctx, 0, initIO)
    go ttyReceive(ctx, ttyc)

    return ttyc
}

func ttyReceive(ctx *QemuContext, tc *ttyContext) {
    buf:= make([]byte, 1)
    for {
        nr,err := tc.vmConn.Read(buf)
        if err != nil {
            glog.Error("reader exit... ", err.Error())
            return
        }

        closed := []uint64{}
        for aid,rd := range tc.subscribers {
            if rd.Stdout == nil {
                continue
            }
            _,err := rd.Stdout.Write(buf[:nr])
            if err != nil {
                glog.V(0).Info("Writer close ", err.Error())
                closed = append(closed, aid)
                continue
            }
        }

        if len(closed) > 0 {
            for _,aid := range closed {
                tc.closeTerm(ctx, aid)
            }
        }
    }
}

func (tc *ttyContext) hasAttachId(attach_id uint64) bool {
    tc.lock.Lock()
    defer tc.lock.Unlock()
    for id,_ := range tc.subscribers {
        if id == attach_id {
            return true
        }
    }
    return false
}

func (tc *ttyContext) findAndClose(ctx *QemuContext, attach_id uint64) bool {
    found := tc.hasAttachId(attach_id)
    if found {
        tc.closeTerm(ctx, attach_id)
    }
    return found
}

func (tc *ttyContext) closeTerm(ctx *QemuContext, attach_id uint64) {
    if tty,ok := tc.subscribers[attach_id]; ok {
        if tty.Stdin != nil {
            tty.Stdin.Close()
        }
        if tty.Stdout != nil {
            tty.Stdout.Close()
        }
        tc.lock.Lock()
        delete(tc.subscribers, attach_id)
        tc.lock.Unlock()
        if tty.ClientTag != "" {
            ctx.clientDereg(tty.ClientTag)
        }
        if tty.Callback != nil {
            tty.Callback <- &types.QemuResponse{
                Code:  types.E_EXEC_FINISH,
                Cause: "Command finished",
                Data:  attach_id,
            }
        }
    }
}

func (tc *ttyContext) connect(ctx *QemuContext, attach_id uint64, tty *TtyIO) error {

    if _,ok := tc.subscribers[attach_id]; ok {
        glog.Errorf("%d has already attached in this tty, cannot connected", attach_id)
        return errors.New("repeat attach a same attach id")
    }

    tc.lock.Lock()
    tc.subscribers[attach_id] = tty
    tc.lock.Unlock()

    if tty.Stdin != nil {
        go func() {
            buf := make([]byte, 1)
            defer tc.closeTerm(ctx, attach_id)
            for {
                nr,err := tty.Stdin.Read(buf)
                if err != nil {
                    glog.Info("a stdin closed, ", err.Error())
                    return
                } else if nr == 1 && buf[0] == ExitChar {
                    glog.Info("got stdin detach char, exit term")
                    return
                }
                _,err = tc.vmConn.Write(buf[:nr])
                if err != nil {
                    glog.Info("vm connection closed, close reader tty, ", err.Error())
                    return
                }
            }
        }()
    }

    return nil
}

func DropAllTty() *TtyIO {
    r,w := io.Pipe()
    go func(){
        buf := make([]byte, 256)
        for {
            _,err := r.Read(buf)
            if err != nil {
                return
            }
        }
    }()
    return &TtyIO{
        Stdin:  nil,
        Stdout: w,
        Callback: nil,
    }
}

func LinerTty(output chan string) *TtyIO {
    r,w := io.Pipe()
    go ttyLiner(r, output)
    return &TtyIO{
        Stdin:  nil,
        Stdout: w,
        Callback: nil,
    }
}

func ttyLiner(conn io.Reader, output chan string) {
    buf     := make([]byte, 1)
    line    := []byte{}
    cr      := false
    emit    := false
    for {

        nr,err := conn.Read(buf)
        if err != nil || nr < 1 {
            glog.V(1).Info("Input byte chan closed, close the output string chan")
            close(output)
            return
        }
        switch buf[0] {
            case '\n':
            emit = !cr
            cr = false
            case '\r':
            emit = true
            cr = true
            default:
            cr = false
            line = append(line, buf[0])
        }
        if emit {
            output <- string(line)
            line = []byte{}
            emit = false
        }
    }
}

func attachSerialPort(ctx *QemuContext, index,addr int) {
    sockName := ctx.serialPortPrefix + strconv.Itoa(index) + ".sock"
    os.Remove(sockName)
    ctx.qmp <- newSerialPortSession(ctx, sockName, index, addr)
//    ctx.qmp <- newSerialPortSession(ctx, sockName, index)

    for i:=0; i < 5; i++ {
        conn, err := net.Dial("unix", sockName)
        if err == nil {
            glog.V(1).Info("connected to ", sockName)
            go connSerialPort(ctx, sockName, conn.(*net.UnixConn), index)
            return
        }
        glog.Warningf("connect %s %d attempt: %s", sockName, i, err.Error())
        time.Sleep(200 * time.Millisecond)
    }

    ctx.hub <- &InitFailedEvent{
        reason: sockName + " init failed ",
    }
}

func connSerialPort(ctx *QemuContext, sockName string, conn *net.UnixConn, index int) {
    tc := setupTty(ctx, sockName, conn, true, DropAllTty())
//    directConnectConsole(ctx, sockName, tc)

    ctx.hub <- &TtyOpenEvent{
        Index:  index,
        TC:     tc,
    }
}

