package parser

import (
	"testing"
)

// ── checkZeroValueIdentifier ─────────────────────────────────────────────────

func TestCheckZeroValueIdentifier_PortZero(t *testing.T) {
	src := []byte(`package main
func main() {
	killByPort(0)
}
`)
	warnings := RunLogicChecks("main.go", src)
	found := findCheck(warnings, "zero_value_id")
	if found == nil {
		t.Fatal("expected zero_value_id warning for killByPort(0)")
	}
	if found.Line != 3 {
		t.Errorf("expected line 3, got %d", found.Line)
	}
}

func TestCheckZeroValueIdentifier_PidZero(t *testing.T) {
	src := []byte(`package main
func main() {
	sendSignalToPid(0, 9)
}
`)
	warnings := RunLogicChecks("main.go", src)
	found := findCheck(warnings, "zero_value_id")
	if found == nil {
		t.Fatal("expected zero_value_id warning for sendSignalToPid(0, ...)")
	}
}

func TestCheckZeroValueIdentifier_NonZero(t *testing.T) {
	src := []byte(`package main
func main() {
	killByPort(8080)
}
`)
	warnings := RunLogicChecks("main.go", src)
	found := findCheck(warnings, "zero_value_id")
	if found != nil {
		t.Error("expected no zero_value_id warning for killByPort(8080)")
	}
}

func TestCheckZeroValueIdentifier_NonIdentifierFunc(t *testing.T) {
	src := []byte(`package main
func main() {
	process(0)
}
`)
	warnings := RunLogicChecks("main.go", src)
	found := findCheck(warnings, "zero_value_id")
	if found != nil {
		t.Error("expected no zero_value_id warning for process(0) — name doesn't contain identifier keywords")
	}
}

func TestCheckZeroValueIdentifier_MethodCall(t *testing.T) {
	src := []byte(`package main
func main() {
	svc.SetPortNumber(0)
}
`)
	warnings := RunLogicChecks("main.go", src)
	found := findCheck(warnings, "zero_value_id")
	if found == nil {
		t.Fatal("expected zero_value_id warning for svc.SetPortNumber(0)")
	}
}

func TestCheckZeroValueIdentifier_CountZero(t *testing.T) {
	src := []byte(`package main
func main() {
	setRetryCount(0)
}
`)
	warnings := RunLogicChecks("main.go", src)
	found := findCheck(warnings, "zero_value_id")
	if found == nil {
		t.Fatal("expected zero_value_id warning for setRetryCount(0)")
	}
}

// ── checkMissingCleanup ──────────────────────────────────────────────────────

func TestCheckMissingCleanup_OpenWithoutClose(t *testing.T) {
	src := []byte(`package main

import "os"

func readFile() {
	f, _ := os.Open("test.txt")
	_ = f
}
`)
	warnings := RunLogicChecks("main.go", src)
	found := findCheck(warnings, "missing_cleanup")
	if found == nil {
		t.Fatal("expected missing_cleanup warning for Open without Close")
	}
}

func TestCheckMissingCleanup_OpenWithDeferClose(t *testing.T) {
	src := []byte(`package main

import "os"

func readFile() {
	f, _ := os.Open("test.txt")
	defer f.Close()
	_ = f
}
`)
	warnings := RunLogicChecks("main.go", src)
	found := findCheck(warnings, "missing_cleanup")
	if found != nil {
		t.Error("expected no missing_cleanup warning when defer Close is present")
	}
}

func TestCheckMissingCleanup_OpenWithClose(t *testing.T) {
	src := []byte(`package main

import "os"

func readFile() {
	f, _ := os.Open("test.txt")
	_ = f
	f.Close()
}
`)
	warnings := RunLogicChecks("main.go", src)
	found := findCheck(warnings, "missing_cleanup")
	if found != nil {
		t.Error("expected no missing_cleanup warning when Close is present")
	}
}

func TestCheckMissingCleanup_LockWithoutUnlock(t *testing.T) {
	src := []byte(`package main

import "sync"

func doWork() {
	var mu sync.Mutex
	mu.Lock()
	_ = mu
}
`)
	warnings := RunLogicChecks("main.go", src)
	found := findCheck(warnings, "missing_cleanup")
	if found == nil {
		t.Fatal("expected missing_cleanup warning for Lock without Unlock")
	}
}

func TestCheckMissingCleanup_LockWithUnlock(t *testing.T) {
	src := []byte(`package main

import "sync"

func doWork() {
	var mu sync.Mutex
	mu.Lock()
	defer mu.Unlock()
	_ = mu
}
`)
	warnings := RunLogicChecks("main.go", src)
	found := findCheck(warnings, "missing_cleanup")
	if found != nil {
		t.Error("expected no missing_cleanup warning when Unlock is present")
	}
}

func TestCheckMissingCleanup_CreateWithoutClose(t *testing.T) {
	src := []byte(`package main

import "os"

func writeFile() {
	f, _ := os.Create("output.txt")
	_ = f
}
`)
	warnings := RunLogicChecks("main.go", src)
	found := findCheck(warnings, "missing_cleanup")
	if found == nil {
		t.Fatal("expected missing_cleanup warning for Create without Close")
	}
}

// ── checkPathExpansion ───────────────────────────────────────────────────────

func TestCheckPathExpansion_TildeInOpen(t *testing.T) {
	src := []byte(`package main

import "os"

func read() {
	os.Open("~/.config/myapp")
}
`)
	warnings := RunLogicChecks("main.go", src)
	found := findCheck(warnings, "path_no_expand")
	if found == nil {
		t.Fatal("expected path_no_expand warning for tilde in os.Open()")
	}
}

func TestCheckPathExpansion_TildeInDial(t *testing.T) {
	src := []byte(`package main

import "net"

func connect() {
	net.Dial("unix", "~/my.sock")
}
`)
	warnings := RunLogicChecks("main.go", src)
	found := findCheck(warnings, "path_no_expand")
	if found == nil {
		t.Fatal("expected path_no_expand warning for tilde in net.Dial()")
	}
}

func TestCheckPathExpansion_NoTilde(t *testing.T) {
	src := []byte(`package main

import "os"

func read() {
	os.Open("/etc/config")
}
`)
	warnings := RunLogicChecks("main.go", src)
	found := findCheck(warnings, "path_no_expand")
	if found != nil {
		t.Error("expected no path_no_expand warning for absolute path")
	}
}

func TestCheckPathExpansion_UserProfile(t *testing.T) {
	src := []byte(`package main

import "os"

func read() {
	os.Open("%USERPROFILE%\\config")
}
`)
	warnings := RunLogicChecks("main.go", src)
	found := findCheck(warnings, "path_no_expand")
	if found == nil {
		t.Fatal("expected path_no_expand warning for USERPROFILE in os.Open()")
	}
}

func TestCheckPathExpansion_NonPathFunction(t *testing.T) {
	src := []byte(`package main

func log() {
	println("~/.config")
}
`)
	warnings := RunLogicChecks("main.go", src)
	found := findCheck(warnings, "path_no_expand")
	if found != nil {
		t.Error("expected no path_no_expand warning for println (not a path function)")
	}
}

// ── checkNilMethodCall ───────────────────────────────────────────────────────

func TestCheckNilMethodCall_BasicCase(t *testing.T) {
	src := []byte(`package main

func doWork() {
	var svc *Service
	svc = nil
	svc.Start()
}

type Service struct{}
func (s *Service) Start() {}
`)
	warnings := RunLogicChecks("main.go", src)
	found := findCheck(warnings, "nil_method_call")
	if found == nil {
		t.Fatal("expected nil_method_call warning for svc.Start() after svc = nil")
	}
}

func TestCheckNilMethodCall_WithNilCheck(t *testing.T) {
	src := []byte(`package main

func doWork() {
	var svc *Service
	svc = nil
	if svc != nil {
		svc.Start()
	}
}

type Service struct{}
func (s *Service) Start() {}
`)
	warnings := RunLogicChecks("main.go", src)
	found := findCheck(warnings, "nil_method_call")
	if found != nil {
		t.Error("expected no nil_method_call warning when nil check is present")
	}
}

func TestCheckNilMethodCall_NonNilAssignment(t *testing.T) {
	src := []byte(`package main

func doWork() {
	svc := NewService()
	svc.Start()
}

type Service struct{}
func (s *Service) Start() {}
func NewService() *Service { return &Service{} }
`)
	warnings := RunLogicChecks("main.go", src)
	found := findCheck(warnings, "nil_method_call")
	if found != nil {
		t.Error("expected no nil_method_call warning for non-nil assignment")
	}
}

// ── checkConcurrentMapWrite ──────────────────────────────────────────────────

func TestCheckConcurrentMapWrite_BasicCase(t *testing.T) {
	src := []byte(`package main

func doWork() {
	m := make(map[string]int)
	go func() {
		m["key"] = 1
	}()
}
`)
	warnings := RunLogicChecks("main.go", src)
	found := findCheck(warnings, "concurrent_map_write")
	if found == nil {
		t.Fatal("expected concurrent_map_write warning for map write inside goroutine")
	}
}

func TestCheckConcurrentMapWrite_WithMutex(t *testing.T) {
	src := []byte(`package main

import "sync"

func doWork() {
	m := make(map[string]int)
	var mu sync.Mutex
	go func() {
		mu.Lock()
		m["key"] = 1
		mu.Unlock()
	}()
}
`)
	warnings := RunLogicChecks("main.go", src)
	found := findCheck(warnings, "concurrent_map_write")
	if found != nil {
		t.Error("expected no concurrent_map_write warning when mutex is used")
	}
}

func TestCheckConcurrentMapWrite_NoGoroutine(t *testing.T) {
	src := []byte(`package main

func doWork() {
	m := make(map[string]int)
	m["key"] = 1
}
`)
	warnings := RunLogicChecks("main.go", src)
	found := findCheck(warnings, "concurrent_map_write")
	if found != nil {
		t.Error("expected no concurrent_map_write warning for map write outside goroutine")
	}
}

func TestCheckConcurrentMapWrite_WithSyncMapStore(t *testing.T) {
	src := []byte(`package main

import "sync"

func doWork() {
	var m sync.Map
	go func() {
		m.Store("key", 1)
	}()
}
`)
	warnings := RunLogicChecks("main.go", src)
	found := findCheck(warnings, "concurrent_map_write")
	if found != nil {
		t.Error("expected no concurrent_map_write warning when sync.Map Store is used")
	}
}

// ── General / edge cases ─────────────────────────────────────────────────────

func TestRunLogicChecks_NonGoFile(t *testing.T) {
	src := []byte(`print("hello")`)
	warnings := RunLogicChecks("main.py", src)
	if len(warnings) != 0 {
		t.Errorf("expected no warnings for non-Go file, got %d", len(warnings))
	}
}

func TestRunLogicChecks_EmptyFile(t *testing.T) {
	src := []byte(`package main`)
	warnings := RunLogicChecks("main.go", src)
	if len(warnings) != 0 {
		t.Errorf("expected no warnings for empty Go file, got %d", len(warnings))
	}
}

func TestRunLogicChecks_EmptyBytes(t *testing.T) {
	warnings := RunLogicChecks("main.go", []byte{})
	if len(warnings) != 0 {
		t.Errorf("expected no warnings for empty bytes, got %d", len(warnings))
	}
}

func TestRunLogicChecks_MultipleWarnings(t *testing.T) {
	src := []byte(`package main

import "os"

func bad() {
	killByPort(0)
	os.Open("~/.config")
	f, _ := os.Create("out.txt")
	_ = f
}
`)
	warnings := RunLogicChecks("main.go", src)
	if len(warnings) < 3 {
		t.Errorf("expected at least 3 warnings, got %d", len(warnings))
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func findCheck(warnings []LogicWarning, check string) *LogicWarning {
	for i := range warnings {
		if warnings[i].Check == check {
			return &warnings[i]
		}
	}
	return nil
}
