// Copyright 2022 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package migrate

import (
	"context"
	"fmt"

	"github.com/dolthub/vitess/go/vt/proto/query"
	"golang.org/x/sync/errgroup"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb/durable"
	"github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/libraries/doltcore/schema"
	"github.com/dolthub/dolt/go/store/datas"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	"github.com/dolthub/dolt/go/store/types"
	"github.com/dolthub/dolt/go/store/val"
)

var (
	flushRef = ref.NewInternalRef("migration-flush")
)

func migrateWorkingSet(ctx context.Context, brRef ref.BranchRef, wsRef ref.WorkingSetRef, old, new *doltdb.DoltDB) error {
	oldWs, err := old.ResolveWorkingSet(ctx, wsRef)
	if err != nil {
		return err
	}

	oldHead, err := old.ResolveCommitRef(ctx, brRef)
	if err != nil {
		return err
	}
	oldHeadRoot, err := oldHead.GetRootValue(ctx)
	if err != nil {
		return err
	}

	newHead, err := new.ResolveCommitRef(ctx, brRef)
	if err != nil {
		return err
	}
	newHeadRoot, err := newHead.GetRootValue(ctx)
	if err != nil {
		return err
	}

	wr, err := migrateRoot(ctx, oldHeadRoot, oldWs.WorkingRoot(), newHeadRoot)
	if err != nil {
		return err
	}

	sr, err := migrateRoot(ctx, oldHeadRoot, oldWs.StagedRoot(), newHeadRoot)
	if err != nil {
		return err
	}

	newWs := doltdb.EmptyWorkingSet(wsRef).WithWorkingRoot(wr).WithStagedRoot(sr)

	return new.UpdateWorkingSet(ctx, wsRef, newWs, hash.Hash{}, oldWs.Meta())
}

func migrateCommit(ctx context.Context, oldCm *doltdb.Commit, new *doltdb.DoltDB, prog Progress) error {
	oldHash, err := oldCm.HashOf()
	if err != nil {
		return err
	}

	ok, err := prog.Has(ctx, oldHash)
	if err != nil {
		return err
	} else if ok {
		return nil
	}

	if oldCm.NumParents() == 0 {
		return migrateInitCommit(ctx, oldCm, new, prog)
	}

	prog.Log(ctx, "migrating commit %s", oldHash.String())

	oldRoot, err := oldCm.GetRootValue(ctx)
	if err != nil {
		return err
	}

	oldParentCm, err := oldCm.GetParent(ctx, 0)
	if err != nil {
		return err
	}
	oldParentRoot, err := oldParentCm.GetRootValue(ctx)
	if err != nil {
		return err
	}

	oph, err := oldParentCm.HashOf()
	if err != nil {
		return err
	}
	ok, err = prog.Has(ctx, oph)
	if err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("cannot find commit mapping for Commit (%s)", oph.String())
	}

	newParentAddr, err := prog.Get(ctx, oph)
	if err != nil {
		return err
	}
	newParentCm, err := new.ReadCommit(ctx, newParentAddr)
	if err != nil {
		return err
	}
	newParentRoot, err := newParentCm.GetRootValue(ctx)
	if err != nil {
		return err
	}

	mRoot, err := migrateRoot(ctx, oldParentRoot, oldRoot, newParentRoot)
	if err != nil {
		return err
	}
	_, addr, err := new.WriteRootValue(ctx, mRoot)
	if err != nil {
		return err
	}
	value, err := new.ValueReadWriter().ReadValue(ctx, addr)
	if err != nil {
		return err
	}

	opts, err := migrateCommitOptions(ctx, oldCm, prog)
	if err != nil {
		return err
	}

	migratedCm, err := new.CommitDangling(ctx, value, opts)
	if err != nil {
		return err
	}

	// update progress
	newHash, err := migratedCm.HashOf()
	if err != nil {
		return err
	}
	if err = prog.Put(ctx, oldHash, newHash); err != nil {
		return err
	}

	// flush ChunkStore
	if err = new.SetHead(ctx, flushRef, newHash); err != nil {
		return err
	}

	// validate root after we flush the ChunkStore to facilitate
	// investigating failed migrations
	if err = validateRootValue(ctx, oldRoot, mRoot); err != nil {
		return err
	}

	return nil
}

func migrateInitCommit(ctx context.Context, cm *doltdb.Commit, new *doltdb.DoltDB, prog Progress) error {
	oldHash, err := cm.HashOf()
	if err != nil {
		return err
	}

	rv, err := doltdb.EmptyRootValue(ctx, new.ValueReadWriter(), new.NodeStore())
	if err != nil {
		return err
	}
	nv := doltdb.HackNomsValuesFromRootValues(rv)

	meta, err := cm.GetCommitMeta(ctx)
	if err != nil {
		return err
	}
	datasDB := doltdb.HackDatasDatabaseFromDoltDB(new)

	creation := ref.NewInternalRef(doltdb.CreationBranch)
	ds, err := datasDB.GetDataset(ctx, creation.String())
	if err != nil {
		return err
	}
	ds, err = datasDB.Commit(ctx, ds, nv, datas.CommitOptions{Meta: meta})
	if err != nil {
		return err
	}

	newCm, err := new.ResolveCommitRef(ctx, creation)
	if err != nil {
		return err
	}
	newHash, err := newCm.HashOf()
	if err != nil {
		return err
	}

	return prog.Put(ctx, oldHash, newHash)
}

func migrateCommitOptions(ctx context.Context, oldCm *doltdb.Commit, prog Progress) (datas.CommitOptions, error) {
	parents, err := oldCm.ParentHashes(ctx)
	if err != nil {
		return datas.CommitOptions{}, err
	}
	if len(parents) == 0 {
		panic("expected non-zero parents list")
	}

	for i := range parents {
		migrated, err := prog.Get(ctx, parents[i])
		if err != nil {
			return datas.CommitOptions{}, err
		}
		parents[i] = migrated
	}

	meta, err := oldCm.GetCommitMeta(ctx)
	if err != nil {
		return datas.CommitOptions{}, err
	}

	return datas.CommitOptions{
		Parents: parents,
		Meta:    meta,
	}, nil
}

func migrateRoot(ctx context.Context, oldParent, oldRoot, newParent *doltdb.RootValue) (*doltdb.RootValue, error) {
	migrated := newParent

	fkc, err := oldRoot.GetForeignKeyCollection(ctx)
	if err != nil {
		return nil, err
	}

	migrated, err = migrated.PutForeignKeyCollection(ctx, fkc)
	if err != nil {
		return nil, err
	}

	err = oldRoot.IterTables(ctx, func(name string, oldTbl *doltdb.Table, sch schema.Schema) (bool, error) {
		ok, err := oldTbl.HasConflicts(ctx)
		if err != nil {
			return true, err
		} else if ok {
			return true, fmt.Errorf("cannot migrate table with conflicts (%s)", name)
		}

		// maybe patch dolt_schemas, dolt docs
		var newSch schema.Schema
		if doltdb.HasDoltPrefix(name) {
			if newSch, err = patchMigrateSchema(ctx, sch); err != nil {
				return true, err
			}
		} else {
			newSch = sch
		}
		if err = validateSchema(newSch); err != nil {
			return true, err
		}

		// if there was a schema change in this commit,
		// diff against an empty table and rewrite everything
		var parentSch schema.Schema

		oldParentTbl, ok, err := oldParent.GetTable(ctx, name)
		if err != nil {
			return true, err
		}
		if ok {
			parentSch, err = oldParentTbl.GetSchema(ctx)
			if err != nil {
				return true, err
			}
		}
		if !ok || !schema.SchemasAreEqual(sch, parentSch) {
			// provide empty table to diff against
			oldParentTbl, err = doltdb.NewEmptyTable(ctx, oldParent.VRW(), oldParent.NodeStore(), sch)
			if err != nil {
				return true, err
			}
		}

		newParentTbl, ok, err := newParent.GetTable(ctx, name)
		if err != nil {
			return true, err
		}
		if !ok || !schema.SchemasAreEqual(sch, parentSch) {
			// provide empty table to diff against
			newParentTbl, err = doltdb.NewEmptyTable(ctx, newParent.VRW(), newParent.NodeStore(), newSch)
			if err != nil {
				return true, err
			}
		}

		mtbl, err := migrateTable(ctx, newSch, oldParentTbl, oldTbl, newParentTbl)
		if err != nil {
			return true, err
		}

		migrated, err = migrated.PutTable(ctx, name, mtbl)
		if err != nil {
			return true, err
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}

	return migrated, nil
}

func migrateTable(ctx context.Context, newSch schema.Schema, oldParentTbl, oldTbl, newParentTbl *doltdb.Table) (*doltdb.Table, error) {
	idx, err := oldParentTbl.GetRowData(ctx)
	if err != nil {
		return nil, err
	}
	oldParentRows := durable.NomsMapFromIndex(idx)

	idx, err = oldTbl.GetRowData(ctx)
	if err != nil {
		return nil, err
	}
	oldRows := durable.NomsMapFromIndex(idx)

	idx, err = newParentTbl.GetRowData(ctx)
	if err != nil {
		return nil, err
	}
	newParentRows := durable.ProllyMapFromIndex(idx)

	oldParentSet, err := oldParentTbl.GetIndexSet(ctx)
	if err != nil {
		return nil, err
	}

	oldSet, err := oldTbl.GetIndexSet(ctx)
	if err != nil {
		return nil, err
	}

	newParentSet, err := newParentTbl.GetIndexSet(ctx)
	if err != nil {
		return nil, err
	}

	var newRows durable.Index
	var newSet durable.IndexSet
	eg, ctx := errgroup.WithContext(ctx)

	eg.Go(func() error {
		newRows, err = migrateIndex(ctx, newSch, oldParentRows, oldRows, newParentRows, newParentTbl.NodeStore())
		return err
	})

	vrw, ns := newParentTbl.ValueReadWriter(), newParentTbl.NodeStore()
	eg.Go(func() error {
		newSet, err = migrateIndexSet(ctx, newSch, oldParentSet, oldSet, newParentSet, vrw, ns)
		return err
	})

	if err = eg.Wait(); err != nil {
		return nil, err
	}

	ai, err := oldTbl.GetAutoIncrementValue(ctx)
	if err != nil {
		return nil, err
	}
	autoInc := types.Uint(ai)

	return doltdb.NewTable(ctx, vrw, ns, newSch, newRows, newSet, autoInc)
}

// patchMigrateSchema attempts to correct irregularities in existing schemas
func patchMigrateSchema(ctx context.Context, existing schema.Schema) (schema.Schema, error) {
	cols := existing.GetAllCols().GetColumns()

	var patched bool
	for i, c := range cols {
		qt := c.TypeInfo.ToSqlType().Type()
		// dolt_schemas and dolt_docs previously written with TEXT columns
		if qt == query.Type_TEXT && c.Kind == types.StringKind {
			cols[i] = schema.NewColumn(c.Name, c.Tag, c.Kind, c.IsPartOfPK, c.Constraints...)
			patched = true
		}
	}
	if !patched {
		return existing, nil
	}

	return schema.SchemaFromCols(schema.NewColCollection(cols...))
}

func migrateIndexSet(
	ctx context.Context,
	sch schema.Schema,
	oldParentSet, oldSet, newParentSet durable.IndexSet,
	vrw types.ValueReadWriter, ns tree.NodeStore,
) (durable.IndexSet, error) {
	newSet := durable.NewIndexSet(ctx, vrw, ns)
	for _, def := range sch.Indexes().AllIndexes() {
		idx, err := oldParentSet.GetIndex(ctx, sch, def.Name())
		if err != nil {
			return nil, err
		}
		oldParent := durable.NomsMapFromIndex(idx)

		idx, err = oldSet.GetIndex(ctx, sch, def.Name())
		if err != nil {
			return nil, err
		}
		old := durable.NomsMapFromIndex(idx)

		idx, err = newParentSet.GetIndex(ctx, sch, def.Name())
		if err != nil {
			return nil, err
		}
		newParent := durable.ProllyMapFromIndex(idx)

		newIdx, err := migrateIndex(ctx, def.Schema(), oldParent, old, newParent, ns)
		if err != nil {
			return nil, err
		}

		newSet, err = newSet.PutIndex(ctx, def.Name(), newIdx)
		if err != nil {
			return nil, err
		}
	}
	return newSet, nil
}

func migrateIndex(
	ctx context.Context,
	sch schema.Schema,
	oldParent, oldMap types.Map,
	newParent prolly.Map,
	ns tree.NodeStore,
) (durable.Index, error) {

	eg, ctx := errgroup.WithContext(ctx)
	differ := make(chan types.ValueChanged, 256)
	writer := make(chan val.Tuple, 256)

	kt, vt := tupleTranslatorsFromSchema(sch, ns)

	// read old noms map
	eg.Go(func() error {
		defer close(differ)
		return oldMap.Diff(ctx, oldParent, differ)
	})

	// translate noms tuples to prolly tuples
	eg.Go(func() error {
		defer close(writer)
		return translateTuples(ctx, kt, vt, differ, writer)
	})

	var newMap prolly.Map
	// write tuples in new prolly map
	eg.Go(func() (err error) {
		newMap, err = writeProllyMap(ctx, newParent, writer)
		return
	})

	if err := eg.Wait(); err != nil {
		return nil, err
	}

	return durable.IndexFromProllyMap(newMap), nil
}

func translateTuples(ctx context.Context, kt, vt translator, differ <-chan types.ValueChanged, writer chan<- val.Tuple) error {
	for {
		var (
			diff   types.ValueChanged
			newKey val.Tuple
			newVal val.Tuple
			ok     bool
			err    error
		)

		select {
		case diff, ok = <-differ:
			if !ok {
				return nil // done
			}
		case _ = <-ctx.Done():
			return ctx.Err()
		}

		switch diff.ChangeType {
		case types.DiffChangeAdded:
			fallthrough

		case types.DiffChangeModified:
			newVal, err = vt.TranslateTuple(ctx, diff.NewValue.(types.Tuple))
			if err != nil {
				return err
			}
			fallthrough

		case types.DiffChangeRemoved:
			newKey, err = kt.TranslateTuple(ctx, diff.Key.(types.Tuple))
			if err != nil {
				return err
			}
		}

		select {
		case writer <- newKey:
		case _ = <-ctx.Done():
			return ctx.Err()
		}

		select {
		case writer <- newVal:
		case _ = <-ctx.Done():
			return ctx.Err()
		}
	}
}

func writeProllyMap(ctx context.Context, prev prolly.Map, writer <-chan val.Tuple) (prolly.Map, error) {
	return prolly.MutateMapWithTupleIter(ctx, prev, channelProvider{tuples: writer})
}

type channelProvider struct {
	tuples <-chan val.Tuple
}

var _ prolly.TupleIter = channelProvider{}

func (p channelProvider) Next(ctx context.Context) (val.Tuple, val.Tuple) {
	var (
		k, v val.Tuple
		ok   bool
	)

	select {
	case k, ok = <-p.tuples:
		if !ok {
			return nil, nil // done
		}
	case _ = <-ctx.Done():
		return nil, nil
	}

	select {
	case v, ok = <-p.tuples:
		assertTrue(ok)
	case _ = <-ctx.Done():
		return nil, nil
	}
	return k, v
}