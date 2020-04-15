// Copyright (C) 2017 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package resolve

import (
	"context"
	"fmt"
	"sort"

	"github.com/google/gapid/core/log"
	"github.com/google/gapid/gapis/api"
	"github.com/google/gapid/gapis/capture"
	"github.com/google/gapid/gapis/database"
	"github.com/google/gapid/gapis/resolve/cmdgrouper"
	"github.com/google/gapid/gapis/service"
	"github.com/google/gapid/gapis/service/path"
)

// CmdGroupData is the additional metadata assigned to api.CmdIDGroups UserData
// field.
type CmdGroupData struct {
	Representation api.CmdID
	// If true, then children frame event groups should not be added to this group.
	NoFrameEventGroups bool
}

// CommandTree resolves the specified command tree path.
func CommandTree(ctx context.Context, c *path.CommandTree, r *path.ResolveConfig) (*service.CommandTree, error) {
	id, err := database.Store(ctx, &CommandTreeResolvable{Path: c, Config: r})
	if err != nil {
		return nil, err
	}
	return &service.CommandTree{
		Root: &path.CommandTreeNode{Tree: path.NewID(id)},
	}, nil
}

type commandTree struct {
	path *path.CommandTree
	root api.CmdIDGroup
}

func (t *commandTree) index(indices []uint64) (api.SpanItem, api.SubCmdIdx) {
	group := api.CmdGroupOrRoot(t.root)
	subCmdRootID := api.SubCmdIdx{}
	for _, idx := range indices {
		switch item := group.Index(idx).(type) {
		case api.CmdIDGroup:
			group = item
		case api.SubCmdRoot:
			// Each SubCmdRoot contains its absolute sub command index.
			subCmdRootID = item.Id
			group = item
		case api.SubCmdIdx:
			id := append(subCmdRootID, item...)
			return id, id
		default:
			return item, subCmdRootID
		}
	}
	return group, subCmdRootID
}

func (t *commandTree) indices(idx []uint64) []uint64 {
	out := []uint64{}
	group := t.root

	for _, id := range idx {
		brk := false
		for {
			if brk {
				break
			}
			i := group.IndexOf(api.CmdID(id))
			out = append(out, i)
			switch item := group.Index(i).(type) {
			case api.CmdIDGroup:
				group = item
			case api.SubCmdRoot:
				group = item.SubGroup
				brk = true
			default:
				return out
			}
		}
	}
	return out
}

// CommandTreeNode resolves the specified command tree node path.
func CommandTreeNode(ctx context.Context, c *path.CommandTreeNode, r *path.ResolveConfig) (*service.CommandTreeNode, error) {
	boxed, err := database.Resolve(ctx, c.Tree.ID())
	if err != nil {
		return nil, err
	}

	cmdTree := boxed.(*commandTree)

	rawItem, absID := cmdTree.index(c.Indices)
	switch item := rawItem.(type) {
	case api.SubCmdIdx:
		return &service.CommandTreeNode{
			Representation: cmdTree.path.Capture.Command(item[0], item[1:]...),
			NumChildren:    0, // TODO: Subcommands
			Commands:       cmdTree.path.Capture.SubCommandRange(item, item),
		}, nil
	case api.CmdIDGroup:
		representation := cmdTree.path.Capture.Command(uint64(item.Range.Last()))
		if data, ok := item.UserData.(*CmdGroupData); ok {
			representation = cmdTree.path.Capture.Command(uint64(data.Representation))
		}

		if len(absID) == 0 {
			// Not a CmdIDGroup under SubCmdRoot, does not contain Subcommands
			return &service.CommandTreeNode{
				Representation: representation,
				NumChildren:    item.Count(),
				Commands:       cmdTree.path.Capture.CommandRange(uint64(item.Range.First()), uint64(item.Range.Last())),
				Group:          item.Name,
				NumCommands:    item.DeepCount(func(g api.CmdIDGroup) bool { return true /* TODO: Subcommands */ }),
			}, nil
		}
		// Is a CmdIDGroup under SubCmdRoot, contains only Subcommands
		startID := append(absID, uint64(item.Range.First()))
		endID := append(absID, uint64(item.Range.Last()))
		representation = cmdTree.path.Capture.Command(endID[0], endID[1:]...)
		return &service.CommandTreeNode{
			Representation: representation,
			NumChildren:    item.Count(),
			Commands:       cmdTree.path.Capture.SubCommandRange(startID, endID),
			Group:          item.Name,
			NumCommands:    item.DeepCount(func(g api.CmdIDGroup) bool { return true /* TODO: Subcommands */ }),
		}, nil

	case api.SubCmdRoot:
		count := uint64(1)
		g := ""
		if len(item.Id) > 1 {
			g = fmt.Sprintf("%v", item.SubGroup.Name)
			count = uint64(item.SubGroup.Count())
		}
		return &service.CommandTreeNode{
			Representation: cmdTree.path.Capture.Command(item.Id[0], item.Id[1:]...),
			NumChildren:    item.SubGroup.Count(),
			Commands:       cmdTree.path.Capture.SubCommandRange(item.Id, item.Id),
			Group:          g,
			NumCommands:    count,
		}, nil
	default:
		panic(fmt.Errorf("Unexpected type: %T, cmdTree.index(c.Indices): (%v, %v), indices: %v",
			item, rawItem, absID, c.Indices))
	}
}

// CommandTreeNodeForCommand returns the path to the CommandTreeNode that
// represents the specified command.
func CommandTreeNodeForCommand(ctx context.Context, p *path.CommandTreeNodeForCommand, r *path.ResolveConfig) (*path.CommandTreeNode, error) {
	boxed, err := database.Resolve(ctx, p.Tree.ID())
	if err != nil {
		return nil, err
	}

	cmdTree := boxed.(*commandTree)

	return &path.CommandTreeNode{
		Tree:    p.Tree,
		Indices: cmdTree.indices(p.Command.Indices),
	}, nil
}

// Resolve builds and returns a *commandTree for the path.CommandTreeNode.
// Resolve implements the database.Resolver interface.
func (r *CommandTreeResolvable) Resolve(ctx context.Context) (interface{}, error) {
	p := r.Path
	ctx = SetupContext(ctx, p.Capture, r.Config)

	c, err := capture.ResolveGraphics(ctx)
	if err != nil {
		return nil, err
	}

	snc, err := SyncData(ctx, p.Capture)
	if err != nil {
		return nil, err
	}

	filter, err := buildFilter(ctx, p.Capture, p.Filter, snc, r.Config)
	if err != nil {
		return nil, err
	}

	groupers := []cmdgrouper.Grouper{}

	if p.GroupByApi {
		groupers = append(groupers, cmdgrouper.Run(
			func(cmd api.Cmd, s *api.GlobalState) (interface{}, string) {
				if api := cmd.API(); api != nil {
					return api.ID(), api.Name()
				}
				return nil, "No context"
			}))
	}

	if p.GroupByThread {
		groupers = append(groupers, cmdgrouper.Run(
			func(cmd api.Cmd, s *api.GlobalState) (interface{}, string) {
				thread := cmd.Thread()
				return thread, fmt.Sprintf("Thread: 0x%x", thread)
			}))
	}

	if p.GroupByUserMarkers {
		groupers = append(groupers, cmdgrouper.Marker())
	}

	// Walk the list of unfiltered commands to build the groups.
	s := c.NewState(ctx)
	err = api.ForeachCmd(ctx, c.Commands, false, func(ctx context.Context, id api.CmdID, cmd api.Cmd) error {
		if err := cmd.Mutate(ctx, id, s, nil, nil); err != nil {
			return fmt.Errorf("Fail to mutate command %v: %v", cmd, err)
		}
		if filter(id, cmd, s) {
			for _, g := range groupers {
				g.Process(ctx, id, cmd, s)
			}
		}
		return nil
	})

	if err != nil {
		return nil, err
	}

	// Build the command tree
	out := &commandTree{
		path: p,
		root: api.CmdIDGroup{
			Name:  "root",
			Range: api.CmdIDRange{End: api.CmdID(len(c.Commands))},
		},
	}
	for _, g := range groupers {
		for _, l := range g.Build(api.CmdID(len(c.Commands))) {
			if group, err := out.root.AddGroup(l.Start, l.End, l.Name); err == nil {
				group.UserData = l.UserData
			}
		}
	}

	if p.GroupByFrame || p.GroupBySubmission {
		events, err := Events(ctx, &path.Events{
			Capture:            p.Capture,
			Filter:             p.Filter,
			FirstInFrame:       true,
			LastInFrame:        true,
			Submissions:        true,
		}, r.Config)
		if err != nil {
			return nil, log.Errf(ctx, err, "Couldn't get events")
		}
		if p.GroupByFrame {
			addFrameGroups(ctx, events, p, out, api.CmdID(len(c.Commands)))
		}
		if p.GroupBySubmission {
			addContainingGroups(ctx, events, p, out, api.CmdID(len(c.Commands)),
				service.EventKind_Submission, "Host Coordination")
		}
	}

	// Now we have all the groups, we finally need to add the filtered commands.
	s = c.NewState(ctx)
	err = api.ForeachCmd(ctx, c.Commands, false, func(ctx context.Context, id api.CmdID, cmd api.Cmd) error {
		if err := cmd.Mutate(ctx, id, s, nil, nil); err != nil {
			return fmt.Errorf("Fail to mutate command %v: %v", cmd, err)
		}

		if !filter(id, cmd, s) {
			return nil
		}

		if v, ok := snc.SubcommandGroups[id]; ok {
			r := out.root.AddRoot([]uint64{uint64(id)}, snc.SubcommandNames)
			// subcommands are added before nesting SubCmdRoots.
			cv := append([]api.SubCmdIdx{}, v...)
			sort.SliceStable(cv, func(i, j int) bool { return len(cv[i]) < len(cv[j]) })
			for _, x := range cv {
				// subcommand marker groups are added before subcommands. And groups with
				// shorter indices are added before groups with longer indices.
				// SubCmdRoot will be created when necessary.
				parentIdx := append([]uint64{uint64(id)}, x[0:len(x)-1]...)
				if snc.SubCommandMarkerGroups.Value(parentIdx) != nil {
					markers := snc.SubCommandMarkerGroups.Value(parentIdx).([]*api.CmdIDGroup)
					r.AddSubCmdMarkerGroups(x[0:len(x)-1], markers, snc.SubcommandNames)
				}
				r.Insert(append([]uint64{}, x...), snc.SubcommandNames)
			}
			return nil
		}

		out.root.AddCommand(id)

		return nil
	})

	if err != nil {
		return nil, err
	}

	// Cluster the commands
	out.root.Cluster(uint64(p.MaxChildren), uint64(p.MaxNeighbours))

	// Set group representations.
	setRepresentations(ctx, &out.root)

	return out, nil
}

// Forms groups out of spans of commands which are not the specified `kind`.
func addContainingGroups(
	ctx context.Context,
	events *service.Events,
	p *path.CommandTree,
	t *commandTree,
	last api.CmdID,
	kind service.EventKind,
	label string) {

	count := 0
	lastLeft := api.CmdID(0)
	for _, e := range events.List {
		i := api.CmdID(e.Command.Indices[0])
		switch e.Kind {
		case kind:
			// Find group which contains this event
			group := &t.root
			for true {
				if idx := group.Spans.IndexOf(i); idx != -1 {
					if subgroup, ok := group.Spans[idx].(*api.CmdIDGroup); ok {
						group = subgroup
						continue
					}
				}
				break
			}

			if data, ok := group.UserData.(*CmdGroupData); ok && data.NoFrameEventGroups {
				continue
			}

			// Start with group of size 1 and grow it backward as long as nothing gets in the way.
			start := i
			for start >= group.Bounds().Start+1 && group.Spans.IndexOf(start-1) == -1 {
				start--
			}
			if lastLeft != 0 && start < lastLeft+1 {
				start = lastLeft + 1
			}
			end := i
			lastLeft = end
			if start < end {
				t.root.AddGroup(start, end, label)
				count++
			}

		case service.EventKind_LastInFrame:
			count = 0
		}
	}
}

func addFrameGroups(ctx context.Context, events *service.Events, p *path.CommandTree, t *commandTree, last api.CmdID) {
	frameCount, frameStart, frameEnd := 0, api.CmdID(0), api.CmdID(0)
	for _, e := range events.List {
		i := api.CmdID(e.Command.Indices[0])
		switch e.Kind {
		case service.EventKind_FirstInFrame:
			frameStart = i

			// If the start is within existing group, move it past the end of the group
			if idx := t.root.Spans.IndexOf(frameStart); idx != -1 {
				span := t.root.Spans[idx]
				if span.Bounds().Start < i { // Unless the start is equal to the group start.
					if subgroup, ok := span.(*api.CmdIDGroup); ok {
						frameStart = subgroup.Range.End
					}
				}
			}

		case service.EventKind_LastInFrame:
			frameCount++
			frameEnd = i

			// If the end is within existing group, move it to the end of the group
			if idx := t.root.Spans.IndexOf(frameEnd); idx != -1 {
				if subgroup, ok := t.root.Spans[idx].(*api.CmdIDGroup); ok {
					frameEnd = subgroup.Range.Last()
				}
			}

			// If the app properly annotates frames as well, we will end up with
			// both groupings, where one is the only child of the other.
			// However, we can not reliably detect this situation as the user
			// group might be surrounded by (potentially filtered) commands.

			group, _ := t.root.AddGroup(frameStart, frameEnd+1, fmt.Sprintf("Frame %v", frameCount))
			if group != nil {
				group.UserData = &CmdGroupData{Representation: i}
			}
		}
	}
	if p.AllowIncompleteFrame && frameCount > 0 && frameStart > frameEnd {
		t.root.AddGroup(frameStart, last, "Incomplete Frame")
	}
}

func setRepresentations(ctx context.Context, g *api.CmdIDGroup) {
	data, _ := g.UserData.(*CmdGroupData)
	if data == nil {
		data = &CmdGroupData{Representation: api.CmdNoID}
		g.UserData = data
	}
	if data.Representation == api.CmdNoID {
		data.Representation = g.Range.Last()
	}

	for _, s := range g.Spans {
		if subgroup, ok := s.(*api.CmdIDGroup); ok {
			setRepresentations(ctx, subgroup)
		}
	}
}
