package main

import (
	"context"
	"fmt"

	"cm2-claude/internal/device"
	"cm2-claude/internal/setup"
)

const keymapUsage = `usage:
  cm2probe keymap show                        print each layer of the active profile
  cm2probe keymap apply <layout.json> [--dry-run]
                                              back up keymap.json, write the layout into its
                                              layer, read it back and verify
  cm2probe keymap restore <backup.json>       back up the current file, then write the backup
flags: --backup-dir DIR (default "backups"), --timeout D (default 15s), plus the open flags of "call"
`

func cmdKeymap(ctx context.Context, env *Env, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(env.Stderr, keymapUsage)
		return exitUsage
	}
	sub, rest := args[0], args[1:]
	fs := newFlagSet(env, "keymap "+sub)
	var of openFlags
	of.add(fs)
	backupDir := fs.String("backup-dir", "backups", "where to save the backup taken before a write")
	dryRun := fs.Bool("dry-run", false, "show the changes, write nothing")
	if err := fs.Set("timeout", "15s"); err != nil {
		return exitError
	}
	pos, err := parseInterleaved(fs, rest)
	if err != nil {
		return exitUsage
	}

	var backup []byte // the keymap to restore
	var layout device.LayoutFile
	switch sub {
	case "show":
		if len(pos) != 0 {
			fmt.Fprint(env.Stderr, keymapUsage)
			return exitUsage
		}
	case "apply":
		if len(pos) != 1 {
			fmt.Fprint(env.Stderr, keymapUsage)
			return exitUsage
		}
		if layout, err = setup.LoadLayout(pos[0]); err != nil {
			fmt.Fprintf(env.Stderr, "cm2probe: %v\n", err)
			return exitError
		}
	case "restore":
		if len(pos) != 1 {
			fmt.Fprint(env.Stderr, keymapUsage)
			return exitUsage
		}
		if backup, err = setup.LoadBackup(pos[0]); err != nil {
			fmt.Fprintf(env.Stderr, "cm2probe: %v\n", err)
			return exitError
		}
	default:
		fmt.Fprint(env.Stderr, keymapUsage)
		return exitUsage
	}

	c, code := connect(env, &of)
	if c == nil {
		return code
	}
	defer c.Close()
	defer report(env, c)

	opts := setup.Options{
		Timeout:    of.timeout,
		BackupDir:  *backupDir,
		DryRun:     *dryRun,
		RestoreCmd: "cm2probe keymap restore",
	}
	switch sub {
	case "show":
		err = setup.Show(ctx, c, of.timeout, env.Stdout)
	case "apply":
		err = setup.Apply(ctx, c, layout, opts, env.Stdout)
	default:
		err = setup.Restore(ctx, c, backup, opts, env.Stdout)
	}
	if err != nil {
		fmt.Fprintf(env.Stderr, "cm2probe: %v\n", err)
		return exitError
	}
	return exitOK
}
