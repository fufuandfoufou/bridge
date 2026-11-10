package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"bridge/internal/project"
	"bridge/internal/store"
)

func taskCmd() *cobra.Command {
	c := &cobra.Command{Use: "task", Short: "Manage the shared task board of the current project"}
	run := func(f func(context.Context, *store.Store, string, []string) error) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			wd, err := os.Getwd()
			if err != nil {
				return err
			}
			st, err := openStore()
			if err != nil {
				return err
			}
			defer st.Close()
			p := project.Identify(wd)
			if err := st.UpsertProject(p); err != nil {
				return err
			}
			return f(cmd.Context(), st, p.ID, args)
		}
	}
	var all bool
	var detail string
	list := &cobra.Command{Use: "list", Short: "List tasks", RunE: run(func(_ context.Context, st *store.Store, pid string, _ []string) error {
		tasks, err := st.ListTasks(pid, all)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "ID\t状态\t认领者\t标题\t备注")
		for _, t := range tasks {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", store.ShortTaskID(t.ID), t.Status, t.Owner, t.Title, t.Note)
		}
		return w.Flush()
	})}
	list.Flags().BoolVar(&all, "all", false, "include finished tasks")
	add := &cobra.Command{Use: "add <title>", Short: "Add a task", Args: cobra.ExactArgs(1), RunE: run(func(_ context.Context, st *store.Store, pid string, a []string) error {
		t, err := st.AddTask(pid, a[0], detail)
		if err == nil {
			fmt.Println(t.ID)
		}
		return err
	})}
	add.Flags().StringVar(&detail, "detail", "", "details")
	reopen := &cobra.Command{Use: "reopen <id>", Short: "Put a claimed task back on the board", Args: cobra.ExactArgs(1), RunE: run(func(_ context.Context, st *store.Store, _ string, a []string) error {
		_, err := st.ReopenTask(a[0])
		return err
	})}
	del := &cobra.Command{Use: "delete <id>", Short: "Delete a task", Args: cobra.ExactArgs(1), RunE: run(func(_ context.Context, st *store.Store, _ string, a []string) error {
		return st.DeleteTask(a[0])
	})}
	c.AddCommand(list, add, reopen, del)
	return c
}
