package image

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/containerd/console"
	"github.com/docker/cli/cli"
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/opts"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/docker/docker/pkg/urlutil"
	controlapi "github.com/moby/buildkit/api/services/control"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/session/auth/authprovider"
	"github.com/moby/buildkit/session/filesync"
	"github.com/moby/buildkit/util/appcontext"
	"github.com/moby/buildkit/util/progress/progressui"
	"github.com/pkg/errors"
	"github.com/tonistiigi/fsutil"
	"golang.org/x/net/context"
	"golang.org/x/sync/errgroup"
)

func runBuildBuildKit(dockerCli command.Cli, options buildOptions) error {
	ctx := appcontext.Context()

	s, err := trySession(dockerCli, options.context)
	if err != nil {
		return err
	}
	if s == nil {
		return errors.Errorf("buildkit not supported by daemon")
	}

	remote := clientSessionRemote
	local := false
	switch {
	case options.contextFromStdin():
		return errors.Errorf("stdin not implemented")
	case isLocalDir(options.context):
		local = true
	case urlutil.IsGitURL(options.context):
		remote = options.context
	case urlutil.IsURL(options.context):
		remote = options.context
	default:
		return errors.Errorf("unable to prepare context: path %q not found", options.context)
	}

	// statusContext, cancelStatus := context.WithCancel(ctx)
	// defer cancelStatus()

	// if span := opentracing.SpanFromContext(ctx); span != nil {
	// 	statusContext = opentracing.ContextWithSpan(statusContext, span)
	// }

	if local {
		s.Allow(filesync.NewFSSyncProvider([]filesync.SyncedDir{
			{
				Name: "context",
				Dir:  options.context,
				Map:  resetUIDAndGID,
			},
			{
				Name: "dockerfile",
				Dir:  filepath.Dir(options.dockerfileName),
			},
		}))
	}

	s.Allow(authprovider.NewDockerAuthProvider())

	eg, ctx := errgroup.WithContext(ctx)

	eg.Go(func() error {
		return s.Run(ctx, dockerCli.Client().DialSession)
	})

	eg.Go(func() error {
		defer func() { // make sure the Status ends cleanly on build errors
			s.Close()
		}()

		configFile := dockerCli.ConfigFile()
		buildOptions := types.ImageBuildOptions{
			Memory:         options.memory.Value(),
			MemorySwap:     options.memorySwap.Value(),
			Tags:           options.tags.GetAll(),
			SuppressOutput: options.quiet,
			NoCache:        options.noCache,
			Remove:         options.rm,
			ForceRemove:    options.forceRm,
			PullParent:     options.pull,
			Isolation:      container.Isolation(options.isolation),
			CPUSetCPUs:     options.cpuSetCpus,
			CPUSetMems:     options.cpuSetMems,
			CPUShares:      options.cpuShares,
			CPUQuota:       options.cpuQuota,
			CPUPeriod:      options.cpuPeriod,
			CgroupParent:   options.cgroupParent,
			Dockerfile:     filepath.Base(options.dockerfileName),
			ShmSize:        options.shmSize.Value(),
			Ulimits:        options.ulimits.GetList(),
			BuildArgs:      configFile.ParseProxyConfig(dockerCli.Client().DaemonHost(), options.buildArgs.GetAll()),
			// AuthConfigs:    authConfigs,
			Labels:        opts.ConvertKVStringsToMap(options.labels.GetAll()),
			CacheFrom:     options.cacheFrom,
			SecurityOpt:   options.securityOpt,
			NetworkMode:   options.networkMode,
			Squash:        options.squash,
			ExtraHosts:    options.extraHosts.GetAll(),
			Target:        options.target,
			RemoteContext: remote,
			Platform:      options.platform,
			SessionID:     "buildkit:" + s.ID(),
		}

		response, err := dockerCli.Client().ImageBuild(ctx, nil, buildOptions)
		if err != nil {
			return err
		}
		defer response.Body.Close()

		t := newTracer()
		var auxCb func(*json.RawMessage)
		if c, err := console.ConsoleFromFile(os.Stderr); err == nil {
			// not using shared context to not disrupt display but let is finish reporting errors
			auxCb = t.write
			eg.Go(func() error {
				return progressui.DisplaySolveStatus(context.TODO(), c, t.displayCh)
			})
			defer close(t.displayCh)
		}
		err = jsonmessage.DisplayJSONMessagesStream(response.Body, os.Stdout, dockerCli.Out().FD(), dockerCli.Out().IsTerminal(), auxCb)
		if err != nil {
			if jerr, ok := err.(*jsonmessage.JSONError); ok {
				// If no error code is set, default to 1
				if jerr.Code == 0 {
					jerr.Code = 1
				}
				// if options.quiet {
				// 	fmt.Fprintf(dockerCli.Err(), "%s%s", progBuff, buildBuff)
				// }
				return cli.StatusError{Status: jerr.Message, StatusCode: jerr.Code}
			}
			return err
		}

		return nil
	})

	return eg.Wait()
}

func resetUIDAndGID(s *fsutil.Stat) bool {
	s.Uid = uint32(0)
	s.Gid = uint32(0)
	return true
}

type tracer struct {
	displayCh chan *client.SolveStatus
}

func newTracer() *tracer {
	return &tracer{
		displayCh: make(chan *client.SolveStatus),
	}
}

func (t *tracer) write(aux *json.RawMessage) {
	var resp controlapi.StatusResponse

	var dt []byte
	if err := json.Unmarshal(*aux, &dt); err != nil {
		return
	}
	if err := (&resp).Unmarshal(dt); err != nil {
		return
	}

	s := client.SolveStatus{}
	for _, v := range resp.Vertexes {
		s.Vertexes = append(s.Vertexes, &client.Vertex{
			Digest:    v.Digest,
			Inputs:    v.Inputs,
			Name:      v.Name,
			Started:   v.Started,
			Completed: v.Completed,
			Error:     v.Error,
			Cached:    v.Cached,
		})
	}
	for _, v := range resp.Statuses {
		s.Statuses = append(s.Statuses, &client.VertexStatus{
			ID:        v.ID,
			Vertex:    v.Vertex,
			Name:      v.Name,
			Total:     v.Total,
			Current:   v.Current,
			Timestamp: v.Timestamp,
			Started:   v.Started,
			Completed: v.Completed,
		})
	}
	for _, v := range resp.Logs {
		s.Logs = append(s.Logs, &client.VertexLog{
			Vertex:    v.Vertex,
			Stream:    int(v.Stream),
			Data:      v.Msg,
			Timestamp: v.Timestamp,
		})
	}

	t.displayCh <- &s
}
