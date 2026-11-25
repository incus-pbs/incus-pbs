package incus

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
)

// Exec runs cmd inside an instance through the Incus API and waits for it.
func (c *Client) Exec(ctx context.Context, project, instance string, cmd []string, env map[string]string) (ExecResult, error) {
	var stdout, stderr bytes.Buffer
	done := make(chan bool)
	op, err := c.p(project).ExecInstance(instance, api.InstanceExecPost{
		Command: cmd, WaitForWS: true, Interactive: false, Environment: env,
	}, &incusclient.InstanceExecArgs{Stdin: bytes.NewReader(nil), Stdout: &stdout, Stderr: &stderr, DataDone: done})
	if err != nil {
		return ExecResult{}, err
	}
	waitErr := make(chan error, 1)
	go func() { waitErr <- op.Wait() }()
	select {
	case <-ctx.Done():
		_ = op.Cancel()
		return ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}, fmt.Errorf("exec in %s/%s: %w", project, instance, ctx.Err())
	case err := <-waitErr:
		if err != nil {
			return ExecResult{}, err
		}
	}
	<-done // all output received
	res := ExecResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: -1}
	if md := op.Get().Metadata; md != nil {
		if rc, ok := md["return"].(float64); ok {
			res.ExitCode = int(rc)
		}
	}
	return res, nil
}

// Importer is the Incus API surface used by restore.
type Importer interface {
	InstanceExists(project, name string) (bool, error)
	VolumeExists(project, pool, name string) (bool, error)
	VolumeInfo(project, pool, name string) (VolumeState, error)
	ProfileExists(project, name string) (bool, error)
	NetworkExists(project, name string) (bool, error)
	GetPool(name string) (Pool, error)
	ImportInstance(ctx context.Context, project, pool, name string, backup io.Reader) error
	ImportVolume(ctx context.Context, project, pool, name string, backup io.Reader) error

	// Used by restore tests on the throwaway instance only.
	StartInstance(ctx context.Context, project, name string) error
	StopInstance(ctx context.Context, project, name string, force bool) error
	DeleteInstance(ctx context.Context, project, name string) error
	// InstanceConfig returns an instance's local config (exists=false if absent).
	InstanceConfig(project, name string) (cfg map[string]string, exists bool, err error)
	// InstanceFile opens a file inside an instance (VMs need a running agent).
	// A missing file is reported as fs.ErrNotExist.
	InstanceFile(project, name, path string) (io.ReadCloser, FileInfo, error)
}

// FileInfo describes a file read with InstanceFile.
type FileInfo struct {
	Type   string // file, directory or symlink
	Target string // symlink target as stored in the link (may be relative)
}

func notFound(err error) bool {
	return err != nil && api.StatusErrorCheck(err, http.StatusNotFound)
}

func exists(err error) (bool, error) {
	switch {
	case err == nil:
		return true, nil
	case notFound(err):
		return false, nil
	}
	return false, err
}

// VolumeState is what restore needs to know about an existing volume.
type VolumeState struct {
	Exists      bool
	ContentType string
	UsedBy      []string
	Shared      bool // security.shared=true allows attaching a block volume more than once
}

// VolumeInfo returns the state of a custom volume.
func (c *Client) VolumeInfo(project, pool, name string) (VolumeState, error) {
	v, _, err := c.p(project).GetStoragePoolVolume(pool, "custom", name)
	if notFound(err) {
		return VolumeState{}, nil
	}
	if err != nil {
		return VolumeState{}, err
	}
	return VolumeState{Exists: true, ContentType: v.ContentType, UsedBy: v.UsedBy, Shared: v.Config["security.shared"] == "true"}, nil
}

// InstanceConfig returns an instance's local config.
func (c *Client) InstanceConfig(project, name string) (map[string]string, bool, error) {
	inst, _, err := c.p(project).GetInstance(name)
	if notFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return inst.Config, true, nil
}

// InstanceExists reports whether an instance exists in a project.
func (c *Client) InstanceExists(project, name string) (bool, error) {
	_, _, err := c.p(project).GetInstance(name)
	return exists(err)
}

// VolumeExists reports whether a custom volume exists.
func (c *Client) VolumeExists(project, pool, name string) (bool, error) {
	_, _, err := c.p(project).GetStoragePoolVolume(pool, "custom", name)
	return exists(err)
}

// ProfileExists reports whether a profile exists in a project.
func (c *Client) ProfileExists(project, name string) (bool, error) {
	_, _, err := c.p(project).GetProfile(name)
	return exists(err)
}

// NetworkExists reports whether a network exists (visible in a project).
func (c *Client) NetworkExists(project, name string) (bool, error) {
	_, _, err := c.p(project).GetNetwork(name)
	return exists(err)
}

// ImportInstance creates an instance from an Incus backup tarball stream.
func (c *Client) ImportInstance(ctx context.Context, project, pool, name string, backup io.Reader) error {
	op, err := c.p(project).CreateInstanceFromBackup(incusclient.InstanceBackupArgs{BackupFile: backup, PoolName: pool, Name: name})
	if err != nil {
		return fmt.Errorf("importing instance: %w", err)
	}
	return waitOp(ctx, op)
}

// ImportVolume creates a custom volume from an Incus volume backup tarball stream.
func (c *Client) ImportVolume(ctx context.Context, project, pool, name string, backup io.Reader) error {
	op, err := c.p(project).CreateStoragePoolVolumeFromBackup(pool, incusclient.StorageVolumeBackupArgs{BackupFile: backup, Name: name})
	if err != nil {
		return fmt.Errorf("importing volume: %w", err)
	}
	return waitOp(ctx, op)
}

func waitOp(ctx context.Context, op incusclient.Operation) error {
	errc := make(chan error, 1)
	go func() { errc <- op.Wait() }()
	select {
	case <-ctx.Done():
		return errors.Join(ctx.Err(), op.Cancel())
	case err := <-errc:
		return err
	}
}

// StartInstance starts an instance and waits for the operation.
func (c *Client) StartInstance(ctx context.Context, project, name string) error {
	op, err := c.p(project).UpdateInstanceState(name, api.InstanceStatePut{Action: "start", Timeout: -1}, "")
	if err != nil {
		return fmt.Errorf("starting %s/%s: %w", project, name, err)
	}
	return waitOp(ctx, op)
}

// StopInstance stops an instance (a no-op if it is not running).
func (c *Client) StopInstance(ctx context.Context, project, name string, force bool) error {
	inst, _, err := c.p(project).GetInstance(name)
	if err != nil {
		return err
	}
	if inst.StatusCode == api.Stopped {
		return nil
	}
	op, err := c.p(project).UpdateInstanceState(name, api.InstanceStatePut{Action: "stop", Timeout: -1, Force: force}, "")
	if err != nil {
		return fmt.Errorf("stopping %s/%s: %w", project, name, err)
	}
	return waitOp(ctx, op)
}

// DeleteInstance deletes a stopped instance.
func (c *Client) DeleteInstance(ctx context.Context, project, name string) error {
	op, err := c.p(project).DeleteInstance(name)
	if err != nil {
		return fmt.Errorf("deleting %s/%s: %w", project, name, err)
	}
	return waitOp(ctx, op)
}

// InstanceFile opens a file inside an instance through the Incus file API.
func (c *Client) InstanceFile(project, name, path string) (io.ReadCloser, FileInfo, error) {
	rc, resp, err := c.p(project).GetInstanceFile(name, path)
	if notFound(err) {
		return nil, FileInfo{}, fmt.Errorf("%s: %w", path, fs.ErrNotExist)
	}
	if err != nil {
		return nil, FileInfo{}, err
	}
	fi := FileInfo{Type: resp.Type}
	if resp.Type != "file" {
		if rc != nil {
			if resp.Type == "symlink" {
				// Incus returns the link target as the body.
				b, _ := io.ReadAll(io.LimitReader(rc, 4096))
				fi.Target = strings.TrimSpace(string(b))
			}
			rc.Close()
		}
		return nil, fi, nil
	}
	return rc, fi, nil
}
