package account

import "sort"

func buildChangeSet(records []*Record, since int, currentRevision int, limit int) *ChangeSet {
	if limit <= 0 {
		limit = 5000
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].Revision == records[j].Revision {
			return records[i].Token < records[j].Token
		}
		return records[i].Revision < records[j].Revision
	})
	hasMore := false
	if len(records) > limit {
		boundaryRevision := records[limit-1].Revision
		end := limit
		for end < len(records) && records[end].Revision == boundaryRevision {
			end++
		}
		hasMore = end < len(records)
		records = records[:end]
	}
	batchMax := since
	var deleted []string
	for _, rec := range records {
		if rec.Revision > batchMax {
			batchMax = rec.Revision
		}
		if rec.IsDeleted() {
			deleted = append(deleted, rec.Token)
		}
	}
	revision := currentRevision
	if hasMore {
		revision = batchMax
	}
	return &ChangeSet{
		Revision:      revision,
		BatchMaxRev:   batchMax,
		Items:         records,
		DeletedTokens: deleted,
		HasMore:       hasMore,
	}
}
