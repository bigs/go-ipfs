package commands

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	oldcmds "github.com/ipfs/go-ipfs/commands"
	lgc "github.com/ipfs/go-ipfs/commands/legacy"
	core "github.com/ipfs/go-ipfs/core"
	e "github.com/ipfs/go-ipfs/core/commands/e"
	corerepo "github.com/ipfs/go-ipfs/core/corerepo"
	config "github.com/ipfs/go-ipfs/repo/config"
	fsrepo "github.com/ipfs/go-ipfs/repo/fsrepo"

	cmds "gx/ipfs/QmTjNRVt2fvaRFu93keEC7z5M1GS1iH6qZ9227htQioTUY/go-ipfs-cmds"
	b58 "gx/ipfs/QmWFAMPqsEyUX7gDUsRVmMWz59FxSpJ1b2v6bJ1yYzo7jY/go-base58-fast/base58"
	ds "gx/ipfs/QmXRKBQA4wXP7xWbFiZsR1GP4HV6wMDQ1aWFxZZ4uBcPX9/go-datastore"
	bstore "gx/ipfs/QmaG4DZ4JaqEfvPWt5nPPgoTzhc1tr1T3f4Nu9Jpdm8ymY/go-ipfs-blockstore"
	cid "gx/ipfs/QmcZfnkapfECQGcLZaf9B79NRg7cRa9EnZh4LSbkCzwNvY/go-cid"
	cmdkit "gx/ipfs/QmceUdzxkimdYsgtX733uNgzf1DLHyBKN6ehGSp85ayppM/go-ipfs-cmdkit"
)

type RepoVersion struct {
	Version string
}

var RepoCmd = &cmds.Command{
	Helptext: cmdkit.HelpText{
		Tagline: "Manipulate the IPFS repo.",
		ShortDescription: `
'ipfs repo' is a plumbing command used to manipulate the repo.
`,
	},

	Subcommands: map[string]*cmds.Command{
		"stat":    repoStatCmd,
		"gc":      lgc.NewCommand(repoGcCmd),
		"fsck":    lgc.NewCommand(RepoFsckCmd),
		"version": lgc.NewCommand(repoVersionCmd),
		"verify":  lgc.NewCommand(repoVerifyCmd),
		"rm-root": repoRmRootCmd,
	},
}

// GcResult is the result returned by "repo gc" command.
type GcResult struct {
	Key   *cid.Cid
	Error string `json:",omitempty"`
}

var repoGcCmd = &oldcmds.Command{
	Helptext: cmdkit.HelpText{
		Tagline: "Perform a garbage collection sweep on the repo.",
		ShortDescription: `
'ipfs repo gc' is a plumbing command that will sweep the local
set of stored objects and remove ones that are not pinned in
order to reclaim hard disk space.
`,
	},
	Options: []cmdkit.Option{
		cmdkit.BoolOption("stream-errors", "Stream errors."),
		cmdkit.BoolOption("quiet", "q", "Write minimal output."),
	},
	Run: func(req oldcmds.Request, res oldcmds.Response) {
		n, err := req.InvocContext().GetNode()
		if err != nil {
			res.SetError(err, cmdkit.ErrNormal)
			return
		}

		streamErrors, _, _ := res.Request().Option("stream-errors").Bool()

		gcOutChan := corerepo.GarbageCollectAsync(n, req.Context())

		outChan := make(chan interface{})
		res.SetOutput(outChan)

		go func() {
			defer close(outChan)

			if streamErrors {
				errs := false
				for res := range gcOutChan {
					if res.Error != nil {
						select {
						case outChan <- &GcResult{Error: res.Error.Error()}:
						case <-req.Context().Done():
							return
						}
						errs = true
					} else {
						select {
						case outChan <- &GcResult{Key: res.KeyRemoved}:
						case <-req.Context().Done():
							return
						}
					}
				}
				if errs {
					res.SetError(fmt.Errorf("encountered errors during gc run"), cmdkit.ErrNormal)
				}
			} else {
				err := corerepo.CollectResult(req.Context(), gcOutChan, func(k *cid.Cid) {
					select {
					case outChan <- &GcResult{Key: k}:
					case <-req.Context().Done():
					}
				})
				if err != nil {
					res.SetError(err, cmdkit.ErrNormal)
				}
			}
		}()
	},
	Type: GcResult{},
	Marshalers: oldcmds.MarshalerMap{
		oldcmds.Text: func(res oldcmds.Response) (io.Reader, error) {
			v, err := unwrapOutput(res.Output())
			if err != nil {
				return nil, err
			}

			quiet, _, err := res.Request().Option("quiet").Bool()
			if err != nil {
				return nil, err
			}

			obj, ok := v.(*GcResult)
			if !ok {
				return nil, e.TypeErr(obj, v)
			}

			if obj.Error != "" {
				fmt.Fprintf(res.Stderr(), "Error: %s\n", obj.Error)
				return nil, nil
			}

			msg := obj.Key.String() + "\n"
			if !quiet {
				msg = "removed " + msg
			}

			return bytes.NewBufferString(msg), nil
		},
	},
}

var repoStatCmd = &cmds.Command{
	Helptext: cmdkit.HelpText{
		Tagline: "Get stats for the currently used repo.",
		ShortDescription: `
'ipfs repo stat' is a plumbing command that will scan the local
set of stored objects and print repo statistics. It outputs to stdout:
NumObjects      int Number of objects in the local repo.
RepoPath        string The path to the repo being currently used.
RepoSize        int Size in bytes that the repo is currently taking.
Version         string The repo version.
`,
	},
	Run: func(req *cmds.Request, res cmds.ResponseEmitter, env cmds.Environment) {
		n, err := GetNode(env)
		if err != nil {
			res.SetError(err, cmdkit.ErrNormal)
			return
		}

		stat, err := corerepo.RepoStat(n, req.Context)
		if err != nil {
			res.SetError(err, cmdkit.ErrNormal)
			return
		}

		cmds.EmitOnce(res, stat)
	},
	Options: []cmdkit.Option{
		cmdkit.BoolOption("human", "Output RepoSize in MiB."),
	},
	Type: corerepo.Stat{},
	Encoders: cmds.EncoderMap{
		cmds.Text: cmds.MakeEncoder(func(req *cmds.Request, w io.Writer, v interface{}) error {
			stat, ok := v.(*corerepo.Stat)
			if !ok {
				return e.TypeErr(stat, v)
			}

			human, _ := req.Options["human"].(bool)

			wtr := tabwriter.NewWriter(w, 0, 0, 1, ' ', 0)

			fmt.Fprintf(wtr, "NumObjects:\t%d\n", stat.NumObjects)
			sizeInMiB := stat.RepoSize / (1024 * 1024)
			if human && sizeInMiB > 0 {
				fmt.Fprintf(wtr, "RepoSize (MiB):\t%d\n", sizeInMiB)
			} else {
				fmt.Fprintf(wtr, "RepoSize:\t%d\n", stat.RepoSize)
			}
			if stat.StorageMax != corerepo.NoLimit {
				maxSizeInMiB := stat.StorageMax / (1024 * 1024)
				if human && maxSizeInMiB > 0 {
					fmt.Fprintf(wtr, "StorageMax (MiB):\t%d\n", maxSizeInMiB)
				} else {
					fmt.Fprintf(wtr, "StorageMax:\t%d\n", stat.StorageMax)
				}
			}
			fmt.Fprintf(wtr, "RepoPath:\t%s\n", stat.RepoPath)
			fmt.Fprintf(wtr, "Version:\t%s\n", stat.Version)
			wtr.Flush()

			return nil

		}),
	},
}

var RepoFsckCmd = &oldcmds.Command{
	Helptext: cmdkit.HelpText{
		Tagline: "Remove repo lockfiles.",
		ShortDescription: `
'ipfs repo fsck' is a plumbing command that will remove repo and level db
lockfiles, as well as the api file. This command can only run when no ipfs
daemons are running.
`,
	},
	Run: func(req oldcmds.Request, res oldcmds.Response) {
		configRoot := req.InvocContext().ConfigRoot

		dsPath, err := config.DataStorePath(configRoot)
		if err != nil {
			res.SetError(err, cmdkit.ErrNormal)
			return
		}

		dsLockFile := filepath.Join(dsPath, "LOCK") // TODO: get this lockfile programmatically
		repoLockFile := filepath.Join(configRoot, fsrepo.LockFile)
		apiFile := filepath.Join(configRoot, "api") // TODO: get this programmatically

		log.Infof("Removing repo lockfile: %s", repoLockFile)
		log.Infof("Removing datastore lockfile: %s", dsLockFile)
		log.Infof("Removing api file: %s", apiFile)

		err = os.Remove(repoLockFile)
		if err != nil && !os.IsNotExist(err) {
			res.SetError(err, cmdkit.ErrNormal)
			return
		}
		err = os.Remove(dsLockFile)
		if err != nil && !os.IsNotExist(err) {
			res.SetError(err, cmdkit.ErrNormal)
			return
		}
		err = os.Remove(apiFile)
		if err != nil && !os.IsNotExist(err) {
			res.SetError(err, cmdkit.ErrNormal)
			return
		}

		res.SetOutput(&MessageOutput{"Lockfiles have been removed.\n"})
	},
	Type: MessageOutput{},
	Marshalers: oldcmds.MarshalerMap{
		cmds.Text: MessageTextMarshaler,
	},
}

var repoRmRootCmd = &cmds.Command{
	Helptext: cmdkit.HelpText{
		Tagline: "Unlink the root used by the files API.",
		ShortDescription: `
'ipfs repo rm-root' will unlink the root used by the files API ('ipfs
files' commands) without trying to read the root itself.  The root and
its children will be removed the next time the garbage collector runs,
unless pinned.

This command is designed to recover form the situation when the root
becomes unavailable and recovering it (such as recreating it, or
fetching it from the network) is not possible.  This command should
only be used as a last resort as using this command could lead to data
loss if there are unpinned nodes connected to the root.

This command can only run when the ipfs daemon is not running.
`,
	},
	Options: []cmdkit.Option{
		cmdkit.BoolOption("confirm", "Really perform operation."),
		cmdkit.BoolOption("remove-local-root", "Remove even if the root exists locally."),
	},
	Run: func(req *cmds.Request, res cmds.ResponseEmitter, env cmds.Environment) {
		confirm, _ := req.Options["confirm"].(bool)
		removeLocalRoot, _ := req.Options["remove-local-root"].(bool)

		if !confirm {
			res.SetError(fmt.Errorf("this is a potentially dangerous operation please pass --confirm to proceed"), cmdkit.ErrNormal)
			return
		}

		ctx, ok := env.(*oldcmds.Context)
		if !ok {
			res.SetError(fmt.Errorf("expected env to be of type %T, got %T", ctx, env), cmdkit.ErrNormal)
			return
		}
		configRoot := ctx.ConfigRoot

		// Can't use a full node as that interferes with the removal
		// of the files root, so open the repo directly
		repo, err := fsrepo.Open(configRoot)
		if err != nil {
			res.SetError(err, cmdkit.ErrNormal)
			return
		}
		defer repo.Close()
		bs := bstore.NewBlockstore(repo.Datastore())

		// Get the old root and display it to the user so that they can
		// can do something to prevent from being garbage collected,
		// such as pin it
		dsk := core.FilesRootKey()
		val, err := repo.Datastore().Get(dsk)
		if err == ds.ErrNotFound || val == nil {
			cmds.EmitOnce(res, &MessageOutput{"Files API root not found.\n"})
			return
		}

		var cidStr string
		var have bool
		c, err := cid.Cast(val.([]byte))
		if err == nil {
			cidStr = c.String()
			have, _ = bs.Has(c)
		} else {
			cidStr = b58.Encode(val.([]byte))
		}

		if have && !removeLocalRoot {
			res.SetError(fmt.Errorf("root %s exists locally. Are you sure you want to unlink this? Pass --remove-local-root to continue", cidStr), cmdkit.ErrNormal)
			return
		} else if !have && removeLocalRoot {
			res.SetError(fmt.Errorf("root does not %s exists locally. Please remove --remove-local-root to continue", cidStr), cmdkit.ErrNormal)
			return
		}

		err = repo.Datastore().Delete(dsk)
		if err != nil {
			res.SetError(fmt.Errorf("unable to remove API root: %s.  Root hash was %s", err.Error(), cidStr), cmdkit.ErrNormal)
			return
		}
		cmds.EmitOnce(res, &MessageOutput{fmt.Sprintf("Unlinked files API root.  Root hash was %s.\n", cidStr)})
	},
	Type: MessageOutput{},
	Encoders: cmds.EncoderMap{
		cmds.Text: cmds.MakeEncoder(func(req *cmds.Request, w io.Writer, v interface{}) error {
			msg, ok := v.(*MessageOutput)
			if !ok {
				return e.TypeErr(msg, v)
			}
			_, err := fmt.Fprintf(w, "%s", msg.Message)
			return err
		}),
	},
}

type VerifyProgress struct {
	Msg      string
	Progress int
}

var repoVerifyCmd = &oldcmds.Command{
	Helptext: cmdkit.HelpText{
		Tagline: "Verify all blocks in repo are not corrupted.",
	},
	Run: func(req oldcmds.Request, res oldcmds.Response) {
		nd, err := req.InvocContext().GetNode()
		if err != nil {
			res.SetError(err, cmdkit.ErrNormal)
			return
		}

		out := make(chan interface{})
		res.SetOutput((<-chan interface{})(out))
		defer close(out)

		bs := bstore.NewBlockstore(nd.Repo.Datastore())
		bs.HashOnRead(true)

		keys, err := bs.AllKeysChan(req.Context())
		if err != nil {
			log.Error(err)
			return
		}

		var fails int
		var i int
		for k := range keys {
			_, err := bs.Get(k)
			if err != nil {
				select {
				case out <- &VerifyProgress{
					Msg: fmt.Sprintf("block %s was corrupt (%s)", k, err),
				}:
				case <-req.Context().Done():
					return
				}
				fails++
			}
			i++
			select {
			case out <- &VerifyProgress{Progress: i}:
			case <-req.Context().Done():
				return
			}
		}

		if fails == 0 {
			select {
			case out <- &VerifyProgress{Msg: "verify complete, all blocks validated."}:
			case <-req.Context().Done():
				return
			}
		} else {
			res.SetError(fmt.Errorf("verify complete, some blocks were corrupt"), cmdkit.ErrNormal)
		}
	},
	Type: &VerifyProgress{},
	Marshalers: oldcmds.MarshalerMap{
		oldcmds.Text: func(res oldcmds.Response) (io.Reader, error) {
			v, err := unwrapOutput(res.Output())
			if err != nil {
				return nil, err
			}

			obj, ok := v.(*VerifyProgress)
			if !ok {
				return nil, e.TypeErr(obj, v)
			}

			buf := new(bytes.Buffer)
			if strings.Contains(obj.Msg, "was corrupt") {
				fmt.Fprintln(os.Stdout, obj.Msg)
				return buf, nil
			}

			if obj.Msg != "" {
				if len(obj.Msg) < 20 {
					obj.Msg += "             "
				}
				fmt.Fprintln(buf, obj.Msg)
				return buf, nil
			}

			fmt.Fprintf(buf, "%d blocks processed.\r", obj.Progress)
			return buf, nil
		},
	},
}

var repoVersionCmd = &oldcmds.Command{
	Helptext: cmdkit.HelpText{
		Tagline: "Show the repo version.",
		ShortDescription: `
'ipfs repo version' returns the current repo version.
`,
	},

	Options: []cmdkit.Option{
		cmdkit.BoolOption("quiet", "q", "Write minimal output."),
	},
	Run: func(req oldcmds.Request, res oldcmds.Response) {
		res.SetOutput(&RepoVersion{
			Version: fmt.Sprint(fsrepo.RepoVersion),
		})
	},
	Type: RepoVersion{},
	Marshalers: oldcmds.MarshalerMap{
		oldcmds.Text: func(res oldcmds.Response) (io.Reader, error) {
			v, err := unwrapOutput(res.Output())
			if err != nil {
				return nil, err
			}
			response, ok := v.(*RepoVersion)
			if !ok {
				return nil, e.TypeErr(response, v)
			}

			quiet, _, err := res.Request().Option("quiet").Bool()
			if err != nil {
				return nil, err
			}

			buf := new(bytes.Buffer)
			if quiet {
				buf = bytes.NewBufferString(fmt.Sprintf("fs-repo@%s\n", response.Version))
			} else {
				buf = bytes.NewBufferString(fmt.Sprintf("ipfs repo version fs-repo@%s\n", response.Version))
			}
			return buf, nil

		},
	},
}
