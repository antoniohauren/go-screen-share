//go:build windows

// Package winaudio captures only the selected process tree using WASAPI loopback.
package winaudio

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32          = windows.NewLazySystemDLL("user32.dll")
	enumWindows     = user32.NewProc("EnumWindows")
	isWindowVisible = user32.NewProc("IsWindowVisible")
	getWindowText   = user32.NewProc("GetWindowTextW")
	getWindowPID    = user32.NewProc("GetWindowThreadProcessId")
	ole32           = windows.NewLazySystemDLL("ole32.dll")
	coInitialize    = ole32.NewProc("CoInitializeEx")
	coUninitialize  = ole32.NewProc("CoUninitialize")
	activateAudio   = windows.NewLazySystemDLL("mmdevapi.dll").NewProc("ActivateAudioInterfaceAsync")
)

type App struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func process(id uint32) (windows.Handle, uint64, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, id)
	if err != nil {
		return 0, 0, err
	}
	var created, exited, kernel, user windows.Filetime
	if err = windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		windows.CloseHandle(h)
		return 0, 0, err
	}
	return h, uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime), nil
}

var enumCallback = syscall.NewCallback(func(hwnd uintptr, apps *[]App) uintptr {
	visible, _, _ := isWindowVisible.Call(hwnd)
	if visible == 0 {
		return 1
	}
	var title [512]uint16
	n, _, _ := getWindowText.Call(hwnd, uintptr(unsafe.Pointer(&title[0])), uintptr(len(title)))
	if n == 0 {
		return 1
	}
	var pid uint32
	getWindowPID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if pid == uint32(os.Getpid()) {
		return 1
	}
	h, created, err := process(pid)
	if err != nil {
		return 1
	}
	windows.CloseHandle(h)
	*apps = append(*apps, App{fmt.Sprintf("%d:%d", pid, created), windows.UTF16ToString(title[:])})
	return 1
})

// ListApps lists visible application windows; sound need not already be playing.
// Creation time prevents a stale selection from capturing a reused process ID.
func ListApps() ([]App, error) {
	apps := []App{}
	ok, _, err := enumWindows.Call(enumCallback, uintptr(unsafe.Pointer(&apps)))
	runtime.KeepAlive(&apps)
	if ok == 0 {
		return nil, fmt.Errorf("list audio apps: %w", err)
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].Name < apps[j].Name })
	return apps, nil
}

type comObject struct{ vtable *[15]uintptr }

//go:uintptrescapes
func (p *comObject) call(method int, args ...uintptr) error {
	values := append([]uintptr{uintptr(unsafe.Pointer(p))}, args...)
	hr, _, _ := syscall.SyscallN(p.vtable[method], values...)
	runtime.KeepAlive(p)
	return hresult(hr)
}

func hresult(hr uintptr) error {
	if int32(hr) < 0 {
		return fmt.Errorf("WASAPI HRESULT 0x%08x", uint32(hr))
	}
	return nil
}

var (
	iidUnknown       = windows.GUID{Data4: [8]byte{0xc0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidAgile         = windows.GUID{Data1: 0x94ea2b94, Data2: 0xe9cc, Data3: 0x49e0, Data4: [8]byte{0xc0, 0xff, 0xee, 0x64, 0xca, 0x8f, 0x5b, 0x90}}
	iidCompletion    = windows.GUID{Data1: 0x41d949ab, Data2: 0x9862, Data3: 0x444a, Data4: [8]byte{0x80, 0xf6, 0xc2, 0x61, 0x33, 0x4d, 0xa5, 0xeb}}
	iidAudioClient   = windows.GUID{Data1: 0x1cb9ad4c, Data2: 0xdbfa, Data3: 0x4c32, Data4: [8]byte{0xb1, 0x78, 0xc2, 0xf5, 0x68, 0xa7, 0x03, 0xb2}}
	iidCaptureClient = windows.GUID{Data1: 0xc8adbd64, Data2: 0xe71e, Data3: 0x48a0, Data4: [8]byte{0xa4, 0xde, 0x18, 0x5c, 0x39, 0x5c, 0xd3, 0x17}}
	activations      sync.Map // Roots callbacks until COM releases its last reference.
)

type activationResult struct {
	client *comObject
	err    error
}
type activation struct {
	vtable  *[4]uintptr
	refs    atomic.Int32
	result  chan activationResult
	params  [3]uint32
	variant struct {
		vt       uint16
		reserved [3]uint16
		size     uint32
		data     uintptr
	}
}

var activationVtable = [4]uintptr{
	syscall.NewCallback(func(this *activation, iid *windows.GUID, out **activation) uintptr {
		guid := *iid
		*out = nil
		if guid != iidUnknown && guid != iidAgile && guid != iidCompletion {
			return 0x80004002
		}
		this.refs.Add(1)
		*out = this
		return 0
	}),
	syscall.NewCallback(func(this *activation) uintptr { return uintptr(this.refs.Add(1)) }),
	syscall.NewCallback(func(this *activation) uintptr {
		n := this.refs.Add(-1)
		if n == 0 {
			activations.Delete(this)
		}
		return uintptr(n)
	}),
	syscall.NewCallback(func(this *activation, operation *comObject) uintptr {
		var unknown, client *comObject
		var hr uint32
		err := operation.call(3, uintptr(unsafe.Pointer(&hr)), uintptr(unsafe.Pointer(&unknown)))
		if err == nil {
			err = hresult(uintptr(hr))
		}
		if unknown != nil {
			if err == nil {
				err = unknown.call(0, uintptr(unsafe.Pointer(&iidAudioClient)), uintptr(unsafe.Pointer(&client)))
			}
			unknown.call(2)
		}
		if err == nil && client == nil {
			err = fmt.Errorf("audio activation returned no client")
		}
		this.result <- activationResult{client, err}
		return 0
	}),
}

func activate(pid uint32) (*comObject, error) {
	a := &activation{vtable: &activationVtable, result: make(chan activationResult, 1), params: [3]uint32{1, pid, 0}}
	a.refs.Store(1)
	a.variant.vt, a.variant.size, a.variant.data = 65, 12, uintptr(unsafe.Pointer(&a.params)) // VT_BLOB, INCLUDE_TARGET_PROCESS_TREE
	activations.Store(a, a)
	defer func() {
		if a.refs.Add(-1) == 0 {
			activations.Delete(a)
		}
	}()
	device, _ := windows.UTF16PtrFromString("VAD\\Process_Loopback")
	var operation *comObject
	hr, _, _ := activateAudio.Call(uintptr(unsafe.Pointer(device)), uintptr(unsafe.Pointer(&iidAudioClient)), uintptr(unsafe.Pointer(&a.variant)), uintptr(unsafe.Pointer(a)), uintptr(unsafe.Pointer(&operation)))
	runtime.KeepAlive(device)
	if err := hresult(hr); err != nil {
		return nil, err
	}
	defer operation.call(2)
	result := <-a.result
	if result.err != nil && result.client != nil {
		result.client.call(2)
		result.client = nil
	}
	return result.client, result.err
}

// Capture emits 48 kHz, stereo, signed 16-bit little-endian PCM. It never opens
// an endpoint or microphone, and never falls back to whole-system loopback.
func Capture(ctx context.Context, id string, ready func(), emit func([]byte)) error {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		return fmt.Errorf("app audio requires 64-bit Windows")
	}
	parts := strings.Split(id, ":")
	if len(parts) != 2 {
		return fmt.Errorf("invalid audio app; refresh and select again")
	}
	pid, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil || pid == 0 {
		return fmt.Errorf("invalid audio process")
	}
	created, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid audio process creation time")
	}
	h, actual, err := process(uint32(pid))
	if err != nil {
		return fmt.Errorf("audio app unavailable: %w", err)
	}
	defer windows.CloseHandle(h)
	if created != actual {
		return fmt.Errorf("audio app restarted; refresh and select again")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	hr, _, _ := coInitialize.Call(0, 0) // COINIT_MULTITHREADED
	if err := hresult(hr); err != nil {
		return err
	}
	defer coUninitialize.Call()
	client, err := activate(uint32(pid))
	if err != nil {
		return fmt.Errorf("process audio activation requires Windows 11: %w", err)
	}
	defer client.call(2)
	// WAVEFORMATEX: PCM, stereo, 48000 Hz, 192000 bytes/sec, 4-byte frames.
	format := struct {
		tag, channels      uint16
		rate, bytes        uint32
		align, bits, extra uint16
	}{1, 2, 48000, 192000, 4, 16, 0}
	if err := client.call(3, 0, 0x80060000, 0, 0, uintptr(unsafe.Pointer(&format)), 0); err != nil {
		return err
	}
	var capture *comObject
	if err := client.call(14, uintptr(unsafe.Pointer(&iidCaptureClient)), uintptr(unsafe.Pointer(&capture))); err != nil {
		return err
	}
	defer capture.call(2)
	event, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(event)
	if err := client.call(13, uintptr(event)); err != nil {
		return err
	}
	if err := client.call(10); err != nil {
		return err
	}
	defer client.call(11)
	ready()
	for ctx.Err() == nil {
		state, err := windows.WaitForSingleObject(h, 0)
		if err != nil {
			return err
		}
		if state == windows.WAIT_OBJECT_0 {
			return fmt.Errorf("audio app closed; select it again to restart sharing")
		}
		if _, err := windows.WaitForSingleObject(event, 50); err != nil {
			return err
		}
		for ctx.Err() == nil {
			var frames uint32
			if err := capture.call(5, uintptr(unsafe.Pointer(&frames))); err != nil {
				return err
			}
			if frames == 0 {
				break
			}
			var data *byte
			var flags uint32
			if err := capture.call(3, uintptr(unsafe.Pointer(&data)), uintptr(unsafe.Pointer(&frames)), uintptr(unsafe.Pointer(&flags)), 0, 0); err != nil {
				return err
			}
			pcm := make([]byte, int(frames)*4)
			if flags&2 == 0 {
				copy(pcm, unsafe.Slice(data, len(pcm)))
			} // AUDCLNT_BUFFERFLAGS_SILENT
			if err := capture.call(4, uintptr(frames)); err != nil {
				return err
			}
			emit(pcm)
		}
	}
	return nil
}
