package main

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/rpc"
)

func costsCommand() *cobra.Command {
	var days int
	var by string
	cmd := &cobra.Command{
		Use:     "costs",
		Short:   "Show what the fleet cost, estimated and billed",
		GroupID: "setup",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if by != "" && by != "box" && by != "project" {
				return fmt.Errorf("--by box or --by project")
			}
			sess, err := connect()
			if err != nil {
				return err
			}
			defer sess.Close()
			var c api.Costs
			if err := rpc.Call(cmd.Context(), sess, api.CostsGet, api.CostsParams{Days: days, By: by}, &c); err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			if by != "" {
				totals := map[string]float64{}
				for _, r := range c.Rows {
					name := r.Box
					if name == "" {
						name = "(fleet: idle workers, disks, addresses, bucket)"
					}
					totals[name] += r.USD
				}
				names := make([]string, 0, len(totals))
				for n := range totals {
					names = append(names, n)
				}
				sort.Slice(names, func(i, j int) bool { return totals[names[i]] > totals[names[j]] })
				fmt.Fprintf(tw, "%s\tESTIMATED (last %d days)\n", map[string]string{"box": "BOX", "project": "PROJECT"}[by], days)
				for _, n := range names {
					fmt.Fprintf(tw, "%s\t$%.2f\n", n, totals[n])
				}
				return tw.Flush()
			}
			type day struct {
				cat     map[string]float64
				billed  float64
				checked bool
			}
			byDay := map[string]*day{}
			get := func(d string) *day {
				if byDay[d] == nil {
					byDay[d] = &day{cat: map[string]float64{}}
				}
				return byDay[d]
			}
			for _, r := range c.Rows {
				get(r.Day).cat[r.Category] += r.USD
			}
			for _, r := range c.Billed {
				d := get(r.Day)
				d.billed += r.USD
				d.checked = true
			}
			ds := make([]string, 0, len(byDay))
			for d := range byDay {
				ds = append(ds, d)
			}
			sort.Sort(sort.Reverse(sort.StringSlice(ds)))
			fmt.Fprintln(tw, "DAY\tCOMPUTE\tSTORAGE\tTRANSFER\tOTHER\tTOTAL\tBILLED")
			for _, d := range ds {
				x := byDay[d]
				total := x.cat["compute"] + x.cat["storage"] + x.cat["transfer"] + x.cat["other"]
				billed := "-"
				if x.checked {
					billed = fmt.Sprintf("$%.2f", x.billed)
				}
				fmt.Fprintf(tw, "%s\t$%.2f\t$%.2f\t$%.2f\t$%.2f\t$%.2f\t%s\n", d, x.cat["compute"], x.cat["storage"], x.cat["transfer"], x.cat["other"], total, billed)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().IntVar(&days, "last", 7, "days to show")
	cmd.Flags().StringVar(&by, "by", "", "total by box or by project")
	return cmd
}
