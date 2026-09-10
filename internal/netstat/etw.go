//go:build windows

package netstat

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The ETW consumer API is not bound by golang.org/x/sys/windows, so the handful
// of calls a real-time session needs are declared here. TRACEHANDLE is a 64-bit
// value passed by value; on a 64-bit target that is one register, which the
// bindings below assume. The line after this comment fails to compile anywhere
// else rather than passing a truncated handle.
var _ = [1]struct{}{}[unsafe.Sizeof(uintptr(0))-8]

var (
	modadvapi32 = windows.NewLazySystemDLL("advapi32.dll")

	procStartTraceW    = modadvapi32.NewProc("StartTraceW")
	procControlTraceW  = modadvapi32.NewProc("ControlTraceW")
	procEnableTraceEx2 = modadvapi32.NewProc("EnableTraceEx2")
	procOpenTraceW     = modadvapi32.NewProc("OpenTraceW")
	procProcessTrace   = modadvapi32.NewProc("ProcessTrace")
	procCloseTrace     = modadvapi32.NewProc("CloseTrace")
)

const (
	wnodeFlagTracedGUID = 0x00020000

	eventTraceRealTimeMode = 0x00000100

	processTraceModeRealTime    = 0x00000100
	processTraceModeEventRecord = 0x10000000

	eventTraceControlQuery = 0
	eventTraceControlStop  = 1

	eventControlCodeEnableProvider = 1

	// traceLevelAll asks the provider for every level it has. The
	// kernel-network events are informational, but the value is a threshold and
	// there is nothing to gain from guessing at it.
	traceLevelAll = 0xff

	invalidProcessTraceHandle = ^uint64(0)
)

// wnodeHeader mirrors WNODE_HEADER.
type wnodeHeader struct {
	BufferSize        uint32
	ProviderID        uint32
	HistoricalContext uint64
	TimeStamp         int64
	GUID              windows.GUID
	ClientContext     uint32
	Flags             uint32
}

// eventTraceProperties mirrors EVENT_TRACE_PROPERTIES. The session and log
// file names live in the same allocation, after the structure, at the offsets
// recorded in the last two fields.
type eventTraceProperties struct {
	Wnode               wnodeHeader
	BufferSize          uint32
	MinimumBuffers      uint32
	MaximumBuffers      uint32
	MaximumFileSize     uint32
	LogFileMode         uint32
	FlushTimer          uint32
	EnableFlags         uint32
	AgeLimit            int32
	NumberOfBuffers     uint32
	FreeBuffers         uint32
	EventsLost          uint32
	BuffersWritten      uint32
	LogBuffersLost      uint32
	RealTimeBuffersLost uint32
	LoggerThreadID      uintptr
	LogFileNameOffset   uint32
	LoggerNameOffset    uint32
}

// eventDescriptor mirrors EVENT_DESCRIPTOR.
type eventDescriptor struct {
	ID      uint16
	Version uint8
	Channel uint8
	Level   uint8
	Opcode  uint8
	Task    uint16
	Keyword uint64
}

// eventHeader mirrors EVENT_HEADER.
type eventHeader struct {
	Size          uint16
	HeaderType    uint16
	Flags         uint16
	EventProperty uint16
	ThreadID      uint32
	ProcessID     uint32
	TimeStamp     int64
	ProviderID    windows.GUID
	Descriptor    eventDescriptor
	ProcessorTime uint64
	ActivityID    windows.GUID
}

// eventRecord mirrors EVENT_RECORD, as delivered to an event record callback.
//
// The two data pointers address the session's own buffers, not Go memory, and
// are typed as pointers so the payload can be read without laundering an
// integer through unsafe.Pointer.
type eventRecord struct {
	Header            eventHeader
	BufferContext     [4]byte
	ExtendedDataCount uint16
	UserDataLength    uint16
	ExtendedData      unsafe.Pointer
	UserData          unsafe.Pointer
	UserContext       uintptr
}

// eventTraceHeader mirrors EVENT_TRACE_HEADER.
type eventTraceHeader struct {
	Size           uint16
	FieldTypeFlags uint16
	Version        uint32
	ThreadID       uint32
	ProcessID      uint32
	TimeStamp      int64
	GUID           windows.GUID
	ProcessorTime  uint64
}

// eventTrace mirrors EVENT_TRACE.
type eventTrace struct {
	Header           eventTraceHeader
	InstanceID       uint32
	ParentInstanceID uint32
	ParentGUID       windows.GUID
	MofData          uintptr
	MofLength        uint32
	ClientContext    uint32
}

// traceLogfileHeader mirrors TRACE_LOGFILE_HEADER.
type traceLogfileHeader struct {
	BufferSize         uint32
	Version            uint32
	ProviderVersion    uint32
	NumberOfProcessors uint32
	EndTime            int64
	TimerResolution    uint32
	MaximumFileSize    uint32
	LogFileMode        uint32
	BuffersWritten     uint32
	LogInstanceGUID    windows.GUID
	LoggerName         uintptr
	LogFileName        uintptr
	TimeZone           windows.Timezoneinformation
	BootTime           int64
	PerfFreq           int64
	StartTime          int64
	ReservedFlags      uint32
	BuffersLost        uint32
}

// eventTraceLogfile mirrors EVENT_TRACE_LOGFILEW.
type eventTraceLogfile struct {
	LogFileName         uintptr
	LoggerName          uintptr
	CurrentTime         int64
	BuffersRead         uint32
	ProcessTraceMode    uint32
	CurrentEvent        eventTrace
	LogfileHeader       traceLogfileHeader
	BufferCallback      uintptr
	BufferSize          uint32
	Filled              uint32
	EventsLost          uint32
	EventRecordCallback uintptr
	IsKernelTrace       uint32
	Context             uintptr
}

// Sizes the structures above must have on a 64-bit Windows target. Checked by a
// test rather than trusted: a field out of place here does not fail loudly, it
// makes OpenTrace reject the structure or, worse, read the callback pointer from
// the wrong slot.
const (
	sizeofEventTraceProperties = 120
	sizeofEventRecord          = 112
	sizeofEventTraceLogfile    = 448
)

// maxSessionName bounds the session name. ETW allows 1024 characters; the
// properties allocation reserves room for the name and a log file name that is
// never set, because StartTrace writes both into the buffer itself.
const maxSessionName = 1024

// newProperties allocates an EVENT_TRACE_PROPERTIES with room for the names
// StartTrace and ControlTrace copy in after it. The backing slice is returned
// so the caller can keep it alive across the system call.
func newProperties() (*eventTraceProperties, []byte) {
	size := sizeofEventTraceProperties + 2*maxSessionName*2
	buf := make([]byte, size)
	p := (*eventTraceProperties)(unsafe.Pointer(&buf[0]))
	p.Wnode.BufferSize = uint32(size)
	p.Wnode.Flags = wnodeFlagTracedGUID
	p.LoggerNameOffset = sizeofEventTraceProperties
	p.LogFileNameOffset = sizeofEventTraceProperties + maxSessionName*2
	return p, buf
}

// etwError turns the status code the trace API returns directly, rather than
// via GetLastError, into an error.
func etwError(call string, status uintptr) error {
	if status == 0 {
		return nil
	}
	return fmt.Errorf("%s: %w", call, windows.Errno(status))
}

func startTrace(name string, props *eventTraceProperties) (uint64, error) {
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	var handle uint64
	r, _, _ := procStartTraceW.Call(
		uintptr(unsafe.Pointer(&handle)),
		uintptr(unsafe.Pointer(n)),
		uintptr(unsafe.Pointer(props)),
	)
	return handle, etwError("StartTrace", r)
}

func controlTrace(handle uint64, name string, props *eventTraceProperties, code uint32) error {
	var n *uint16
	if name != "" {
		var err error
		if n, err = windows.UTF16PtrFromString(name); err != nil {
			return err
		}
	}
	r, _, _ := procControlTraceW.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(n)),
		uintptr(unsafe.Pointer(props)),
		uintptr(code),
	)
	return etwError("ControlTrace", r)
}

func enableProvider(handle uint64, provider *windows.GUID, level uint8, anyKeyword uint64) error {
	r, _, _ := procEnableTraceEx2.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(provider)),
		eventControlCodeEnableProvider,
		uintptr(level),
		uintptr(anyKeyword),
		0, // MatchAllKeyword
		0, // Timeout: return without waiting for the provider to acknowledge
		0, // EnableParameters
	)
	return etwError("EnableTraceEx2", r)
}

func openTrace(logfile *eventTraceLogfile) (uint64, error) {
	r, _, e := procOpenTraceW.Call(uintptr(unsafe.Pointer(logfile)))
	if uint64(r) == invalidProcessTraceHandle {
		if e == nil || e == windows.ERROR_SUCCESS {
			return 0, fmt.Errorf("OpenTrace: failed")
		}
		return 0, fmt.Errorf("OpenTrace: %w", e)
	}
	return uint64(r), nil
}

// processTrace blocks, delivering events to the callback registered on the
// trace, until the session is stopped or the trace handle is closed.
func processTrace(handle uint64) error {
	r, _, _ := procProcessTrace.Call(
		uintptr(unsafe.Pointer(&handle)),
		1,
		0,
		0,
	)
	return etwError("ProcessTrace", r)
}

func closeTrace(handle uint64) error {
	r, _, _ := procCloseTrace.Call(uintptr(handle))
	// ERROR_CTX_CLOSE_PENDING means the close completes once ProcessTrace
	// returns, which is the ordinary outcome when closing from another thread.
	if windows.Errno(r) == windows.ERROR_CTX_CLOSE_PENDING {
		return nil
	}
	return etwError("CloseTrace", r)
}
