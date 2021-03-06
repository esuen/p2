package hooks

import (
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	"bytes"

	. "github.com/anthonybishopric/gotcha"
	"github.com/square/p2/pkg/logging"
	"github.com/square/p2/pkg/manifest"
	"github.com/square/p2/pkg/pods"
)

const podId = "TestPod"

var hooksRequiredEmpty []string

func TestExecutableHooksAreRun(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "hook")
	Assert(t).IsNil(err, "the error should have been nil")
	defer os.RemoveAll(tempDir)

	podDir, err := ioutil.TempDir("", "pod")
	defer os.RemoveAll(podDir)
	Assert(t).IsNil(err, "the error should have been nil")

	ioutil.WriteFile(path.Join(tempDir, "test1"), []byte("#!/bin/sh\necho $HOOKED_POD_ID > $(dirname $0)/output"), 0755)

	// So PodFromPodHome doesn't bail out, write a minimal current_manifest.yaml
	ioutil.WriteFile(path.Join(podDir, "current_manifest.yaml"), []byte("id: my_hook"), 0755)

	hooks := NewContext(tempDir, pods.DefaultPath, &logging.DefaultLogger, NewFileAuditLogger(&logging.DefaultLogger))
	pod, err := pods.PodFromPodHome("testNode", podDir)
	Assert(t).IsNil(err, "the error should have been nil")
	hooks.runHooks(tempDir, AfterInstall, pod, testManifest(), logging.DefaultLogger, hooksRequiredEmpty)

	contents, err := ioutil.ReadFile(path.Join(tempDir, "output"))
	Assert(t).IsNil(err, "the error should have been nil")

	Assert(t).AreEqual(string(contents), "TestPod\n", "hook should output pod ID into output file")
}

func TestNonExecutableHooksAreNotRun(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "hook")
	Assert(t).IsNil(err, "the error should have been nil")
	defer os.RemoveAll(tempDir)

	podDir, err := ioutil.TempDir("", "pod")
	defer os.RemoveAll(podDir)
	Assert(t).IsNil(err, "the error should have been nil")

	err = ioutil.WriteFile(path.Join(tempDir, "test2"), []byte("#!/bin/sh\ntouch $(dirname $0)/failed"), 0644)
	Assert(t).IsNil(err, "the error should have been nil")

	// So PodFromPodHome doesn't bail out, write a minimal current_manifest.yaml
	ioutil.WriteFile(path.Join(podDir, "current_manifest.yaml"), []byte("id: my_hook"), 0755)

	hooks := NewContext(tempDir, pods.DefaultPath, &logging.DefaultLogger, NewFileAuditLogger(&logging.DefaultLogger))
	pod, err := pods.PodFromPodHome("testNode", podDir)
	Assert(t).IsNil(err, "the error should have been nil")
	hooks.runHooks(tempDir, AfterInstall, pod, testManifest(), logging.DefaultLogger, hooksRequiredEmpty)

	if _, err := os.Stat(path.Join(tempDir, "failed")); err == nil {
		t.Fatal("`failed` file exists; non-executable hook ran but should not have run")
	}
}

func TestDirectoriesDoNotBreakEverything(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "hook")
	Assert(t).IsNil(err, "the error should have been nil")
	defer os.RemoveAll(tempDir)

	podDir, err := ioutil.TempDir("", "pod")
	defer os.RemoveAll(podDir)
	Assert(t).IsNil(err, "the error should have been nil")

	Assert(t).IsNil(os.Mkdir(path.Join(tempDir, "mydir"), 0755), "Should not have erred")

	// So PodFromPodHome doesn't bail out, write a minimal current_manifest.yaml
	ioutil.WriteFile(path.Join(podDir, "current_manifest.yaml"), []byte("id: my_hook"), 0755)

	pod, err := pods.PodFromPodHome("testNode", podDir)
	Assert(t).IsNil(err, "the error should have been nil")
	hooks := NewContext(tempDir, pods.DefaultPath, &logging.DefaultLogger, NewFileAuditLogger(&logging.DefaultLogger))
	err = hooks.runHooks(tempDir, AfterInstall, pod, testManifest(), logging.DefaultLogger, hooksRequiredEmpty)

	Assert(t).IsNil(err, "Got an error when running a directory inside the hooks directory")
}

func testManifest() manifest.Manifest {
	builder := manifest.NewBuilder()
	builder.SetID(podId)
	return builder.GetManifest()
}

func testReqHooksManifest() manifest.Manifest {
	builder := manifest.NewBuilder()
	builder.SetID(podId)
	reqHooks := []string{"basename_req-hook", "basename_req-hook2"}
	config := map[interface{}]interface{}{"required_hooks": reqHooks}
	builder.SetConfig(config)
	return builder.GetManifest()
}

func TestHookRunWithTimeout(t *testing.T) {
	timeout := 1 * time.Second
	sleep := "5" // Wait longer than hook timeout to trigger failure

	// build an executable file to feed to Hook
	contents := []byte("#!/usr/bin/env sh\nsleep " + sleep)

	tmpFile, err := tempFileWithContents("test-hook-run-with-timeout.", contents)
	if err != nil {
		t.Error(err.Error())
	}
	defer os.Remove(tmpFile)

	logger := logging.TestLogger()
	hook := NewHookExecContext(tmpFile, "timeout-test-hook", timeout, HookExecutionEnvironment{}, logger)

	toErr := hook.RunWithTimeout(logger)
	if _, ok := toErr.(ErrHookTimeout); !ok {
		// we either had no error or a different error
		t.Errorf("timeout did not throw a HookTimeoutError: timeout: %#v / sleep: %#v / err: %#v", timeout, sleep, toErr)
	}
}

func TestHookAuditLogging(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "hook")
	Assert(t).IsNil(err, "the error should have been nil")
	defer os.RemoveAll(tempDir)

	podDir, err := ioutil.TempDir("", "pod")
	defer os.RemoveAll(podDir)
	Assert(t).IsNil(err, "the error should have been nil")

	err = ioutil.WriteFile(path.Join(tempDir, "test1"), []byte("#!/bin/sh\necho $HOOKED_POD_ID > $(dirname $0)/output"), 0755)
	Assert(t).IsNil(err, "Caught error while writing test hook")

	// So PodFromPodHome doesn't bail out, write a minimal current_manifest.yaml
	ioutil.WriteFile(path.Join(podDir, "current_manifest.yaml"), []byte("id: my_hook"), 0755)
	Assert(t).IsNil(err, "Caught error while writing test manifest")

	auditLoggerLogger := logging.TestLogger()
	buf := &bytes.Buffer{}
	auditLoggerLogger.Logger.Out = buf

	hooks := NewContext(tempDir, pods.DefaultPath, &logging.DefaultLogger, NewFileAuditLogger(&auditLoggerLogger))
	pod, err := pods.PodFromPodHome("testNode", podDir)
	Assert(t).IsNil(err, "the error should have been nil")
	hooks.runHooks(tempDir, AfterInstall, pod, testManifest(), logging.DefaultLogger, hooksRequiredEmpty)

	Assert(t).IsTrue(len(buf.Bytes()) > 0, "Expected buf to capture audit logs.")

	contents, err := ioutil.ReadFile(path.Join(tempDir, "output"))
	Assert(t).IsNil(err, "the error should have been nil")

	Assert(t).AreEqual(string(contents), "TestPod\n", "hook should output pod ID into output file")
}

func TestRequiredHooksAreFatal(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "hook")
	Assert(t).IsNil(err, "the error should have been nil")
	defer os.RemoveAll(tempDir)

	podDir, err := ioutil.TempDir("", "pod")
	defer os.RemoveAll(podDir)
	Assert(t).IsNil(err, "the error should have been nil")

	hooksRequired := []string{"basename_req-hook"}

	ioutil.WriteFile(path.Join(tempDir, "basename_req-hook"), []byte("#!/usr/bin/env sh\nexit 1"), 0755)

	// So PodFromPodHome doesn't bail out, write a minimal current_manifest.yaml
	ioutil.WriteFile(path.Join(podDir, "current_manifest.yaml"), []byte("id: reqhooks"), 0755)

	hooks := NewContext(tempDir, pods.DefaultPath, &logging.DefaultLogger, NewFileAuditLogger(&logging.DefaultLogger))
	pod, err := pods.PodFromPodHome("testNode", podDir)
	Assert(t).IsNil(err, "the error should have been nil")
	err = hooks.runHooks(tempDir, AfterInstall, pod, testReqHooksManifest(), logging.DefaultLogger, hooksRequired)

	Assert(t).IsNotNil(err, "An error should have been returned from runHooks")
}

func TestOptHooksAreNotFatal(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "hook")
	Assert(t).IsNil(err, "the error should have been nil")
	defer os.RemoveAll(tempDir)

	podDir, err := ioutil.TempDir("", "pod")
	defer os.RemoveAll(podDir)
	Assert(t).IsNil(err, "the error should have been nil")

	hooksRequired := []string{"basename_req-hook"}

	ioutil.WriteFile(path.Join(tempDir, "basename_opt-hook"), []byte("#!/usr/bin/env sh\nexit 1"), 0755)

	// So PodFromPodHome doesn't bail out, write a minimal current_manifest.yaml
	ioutil.WriteFile(path.Join(podDir, "current_manifest.yaml"), []byte("id: opt_hook"), 0755)

	hooks := NewContext(tempDir, pods.DefaultPath, &logging.DefaultLogger, NewFileAuditLogger(&logging.DefaultLogger))
	pod, err := pods.PodFromPodHome("testNode", podDir)
	Assert(t).IsNil(err, "the error should have been nil")
	err = hooks.runHooks(tempDir, AfterInstall, pod, testManifest(), logging.DefaultLogger, hooksRequired)

	Assert(t).IsNil(err, "No error should have been returned")
}

// tempFileWithContents creates a tempfile (0744), fills it with contents and returns the path to it
//
// You are expected to delete the file afterwards
func tempFileWithContents(name string, contents []byte) (string, error) {
	tmpfile, err := ioutil.TempFile(".", name)
	if err != nil {
		return "", err
	}

	os.Chmod(tmpfile.Name(), 0744)

	tmpfile.Write([]byte(contents))
	tmpfile.Close()

	path, _ := filepath.Abs(tmpfile.Name())

	return path, nil
}
