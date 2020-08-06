// Copyright (c) 2020 Blockwatch Data Inc.
// Author: alex@blockwatch.cc

package index

import (
	"context"
	"fmt"
	"github.com/jinzhu/gorm"
	"sort"
	"tezos_index/chain"
	"tezos_index/puller/models"
	"tezos_index/rpc"
)

type RightsIndex struct {
	db *gorm.DB
}

func NewRightsIndex(db *gorm.DB) *RightsIndex {
	return &RightsIndex{db}
}

func (idx *RightsIndex) DB() *gorm.DB {
	return idx.db
}

func (idx *RightsIndex) ConnectBlock(ctx context.Context, block *models.Block, builder models.BlockBuilder) error {
	upd := make([]*models.Right, 0, 32+block.Priority+block.NSeedNonce)
	// load and update rights when seed nonces are published
	if block.NSeedNonce > 0 {
		for _, v := range block.Ops {
			if v.Type != chain.OpTypeSeedNonceRevelation {
				continue
			}
			// find and type-cast the seed nonce op
			op, ok := block.GetRPCOp(v.OpN, v.OpC)
			if !ok {
				return fmt.Errorf("rights: missing seed nonce op [%d:%d]", v.OpN, v.OpC)
			}
			sop, ok := op.(*rpc.SeedNonceOp)
			if !ok {
				return fmt.Errorf("rights: seed nonce op [%d:%d]: unexpected type %T ", v.OpN, v.OpC, op)
			}
			// seed nonces are injected by the current block's baker, but may originate
			// from another baker who was required to publish them as message into the
			// network
			var updd []*models.Right
			err := idx.DB().Where("height = ? and type = ? and is_seed_required = ?",
				sop.Level, int64(chain.RightTypeBaking), true).Find(&updd).Error
			if err != nil {
				return fmt.Errorf("rights: seed nonce right %s %d: %v", block.Baker, sop.Level, err)
			}
			for _, val := range updd {
				val.IsSeedRevealed = true
				upd = append(upd, val)
			}
		}
	}

	// update baking and endorsing rights
	// careful: rights is slice of structs, not pointers
	rights := builder.Rights(chain.RightTypeBaking)
	for i := range rights {
		pd := rights[i].Priority - block.Priority
		if pd > 0 {
			continue
		}
		rights[i].IsLost = pd < 0
		if pd == 0 {
			rights[i].IsStolen = block.Priority > 0
			rights[i].IsSeedRequired = block.Height%block.Params.BlocksPerCommitment == 0
		}
		upd = append(upd, &(rights[i]))
	}

	// endorsing rights are for parent block
	if block.Parent != nil {
		if missed := ^block.Parent.SlotsEndorsed; missed > 0 {
			// careful: rights is slice of structs, not pointers
			rights := builder.Rights(chain.RightTypeEndorsing)
			for i := range rights {
				if missed&(0x1<<uint(rights[i].Priority)) == 0 {
					continue
				}
				rights[i].IsMissed = true
				upd = append(upd, &(rights[i]))
			}
		}
	}

	// todo batch update
	tx := idx.DB().Begin()
	for _, up := range upd {
		if err := tx.Model(&models.Right{}).Updates(up).Error; err != nil {
			tx.Rollback()
			return err
		}
	}
	tx.Commit()

	// nothing more to do when no new rights are available
	if len(block.TZ.Baking) == 0 && len(block.TZ.Endorsing) == 0 {
		return nil
	}

	// insert all baking rights for a cycle, then all endorsing rights
	ins := make([]*models.Right, 0, (64+32)*block.Params.BlocksPerCycle)
	for _, v := range block.TZ.Baking {
		acc, ok := builder.AccountByAddress(v.Delegate)
		if !ok {
			return fmt.Errorf("rights: missing baker account %s", v.Delegate)
		}
		ins = append(ins, &models.Right{
			Type:      chain.RightTypeBaking,
			Height:    v.Level,
			Cycle:     block.Params.CycleFromHeight(v.Level),
			Priority:  v.Priority,
			AccountId: acc.RowId,
		})
	}
	// sort endorsing rights by slot, they are only sorted by height here
	height := block.TZ.Endorsing[0].Level
	erights := make([]*models.Right, 0, block.Params.EndorsersPerBlock)
	for _, v := range block.TZ.Endorsing {
		// sort and flush into insert
		if v.Level > height {
			sort.Slice(erights, func(i, j int) bool { return erights[i].Priority < erights[j].Priority })
			for _, r := range erights {
				ins = append(ins, r)
			}
			erights = erights[:0]
			height = v.Level
		}
		acc, ok := builder.AccountByAddress(v.Delegate)
		if !ok {
			return fmt.Errorf("rights: missing endorser account %s", v.Delegate)
		}
		for _, slot := range sort.IntSlice(v.Slots) {
			erights = append(erights, &models.Right{
				Type:      chain.RightTypeEndorsing,
				Height:    v.Level,
				Cycle:     block.Params.CycleFromHeight(v.Level),
				Priority:  slot,
				AccountId: acc.RowId,
			})
		}
	}
	// sort and flush the last bulk
	sort.Slice(erights, func(i, j int) bool { return erights[i].Priority < erights[j].Priority })
	for _, r := range erights {
		ins = append(ins, r)
	}

	// todo batch insert
	tx = idx.DB().Begin()
	for _, v := range ins {
		if err := tx.Create(v).Error; err != nil {
			tx.Rollback()
			return err
		}
	}
	tx.Commit()
	return nil
}

func (idx *RightsIndex) DisconnectBlock(ctx context.Context, block *models.Block, builder models.BlockBuilder) error {
	// reverse right updates
	upd := make([]*models.Right, 0, 32+block.Priority+block.NSeedNonce)
	// load and update rights when seed nonces are published
	if block.NSeedNonce > 0 {
		for _, v := range block.Ops {
			if v.Type != chain.OpTypeSeedNonceRevelation {
				continue
			}
			// find and type-cast the seed nonce op
			op, ok := block.GetRPCOp(v.OpN, v.OpC)
			if !ok {
				return fmt.Errorf("rights: missing seed nonce op [%d:%d]", v.OpN, v.OpC)
			}
			sop, ok := op.(*rpc.SeedNonceOp)
			if !ok {
				return fmt.Errorf("rights: seed nonce op [%d:%d]: unexpected type %T ", v.OpN, v.OpC, op)
			}
			// seed nonces are injected by the current block's baker!
			// we assume each baker has only one priority level per block
			var tmps []*models.Right
			err := idx.DB().Where("height = ? and type = ? and account_id = ?",
				sop.Level, int64(chain.RightTypeBaking), block.Baker.RowId.Value()).Find(&tmps).Error
			if err != nil {
				return fmt.Errorf("rights: seed nonce right %s %d: %v", block.Baker, sop.Level, err)
			}
			for _, tmp := range tmps {
				tmp.IsSeedRevealed = false
				upd = append(upd, tmp)
			}
		}
	}

	// update baking and endorsing rights
	if block.Priority > 0 {
		// careful: rights is slice of structs, not pointers
		rights := builder.Rights(chain.RightTypeBaking)
		for i := range rights {
			rights[i].IsLost = false
			rights[i].IsStolen = false
			upd = append(upd, &(rights[i]))
		}
	}
	// endorsing rights are for parent block
	// careful: rights is slice of structs, not pointers
	rights := builder.Rights(chain.RightTypeEndorsing)
	for i := range rights {
		rights[i].IsMissed = false
		upd = append(upd, &(rights[i]))
	}

	// todo batch update
	tx := idx.DB().Begin()
	for _, val := range upd {
		if err := tx.Model(&models.Right{}).Updates(val).Error; err != nil {
			tx.Rollback()
			return err
		}
	}
	tx.Commit()

	// new rights are fetched in cycles
	if block.Params.IsCycleStart(block.Height) {
		return idx.DeleteCycle(ctx, block.Height)
	}
	return nil
}

func (idx *RightsIndex) DeleteBlock(ctx context.Context, height int64) error {
	return nil
}

func (idx *RightsIndex) DeleteCycle(ctx context.Context, cycle int64) error {
	log.Debugf("Rollback deleting rights for cycle %d", cycle)
	return idx.DB().Where("cycle = ?", cycle).Delete(&models.Right{}).Error
}