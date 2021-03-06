// Copyright (c) 2018 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file.
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

package snapshotter

import (
	"path"
	"time"

	"github.com/gardener/etcd-backup-restore/pkg/snapstore"
)

// GarbageCollector basically consider the older backups as garbage and deletes it
func (ssr *Snapshotter) GarbageCollector(stopCh <-chan bool) {
	for {
		select {
		case <-stopCh:
			ssr.logger.Infoln("GC: Stop signal received. Closing garbage collector.")
			return
		case <-time.After(ssr.garbageCollectionPeriodSeconds * time.Second):
			ssr.logger.Infoln("GC: Executing garbage collection...")
			snapList, err := ssr.store.List()
			if err != nil {
				ssr.logger.Warnf("GC: Failed to list snapshots: %v", err)
				continue
			}

			// At this stage, we assume the snapList is sorted in increasing order of time, i.e. older snapshot at
			// lower index and newer snapshot at higher index in list.
			snapLen := len(snapList)
			var snapStreamIndexList []int
			// snapStream indicates the list of snapshot, where first snapshot is base/full snapshot followed by
			// list of incremental snapshots based on it. snapStreamIndex points to index of snapStream in snapList
			// which consist of collection of snapStream.
			snapStreamIndexList = append(snapStreamIndexList, 0)
			for index := 1; index < snapLen; index++ {
				if snapList[index].Kind == snapstore.SnapshotKindFull {
					snapStreamIndexList = append(snapStreamIndexList, index)
				}
			}

			switch ssr.garbageCollectionPolicy {
			case GarbageCollectionPolicyExponential:
				// Overall policy:
				// Delete delta snapshots in all snapStream but the latest one.
				// Keep only the last 24 hourly backups and of all other backups only the last backup in a day.
				// Keep only the last 7 daily backups and of all other backups only the last backup in a week.
				// Keep only the last 4 weekly backups.

				now := time.Now().UTC()
				var (
					deleteSnap  = true
					backupMode  = "None"
					backupCount = -1
					// Limit here indicates the number of snapshot to retain in particular mode.
					hourModeLimit = 24
					dayModeLimit  = 7
					weekModeLimit = 4
				)
				// Here we start loop from len(snapStreamIndexList) - 2, because we want to keep last snapstream
				// including delta snapshots in it.
				for snapStreamIndex := len(snapStreamIndexList) - 2; snapStreamIndex >= 0; snapStreamIndex-- {
					snap := snapList[snapStreamIndexList[snapStreamIndex]]
					// garbage collect delta snapshots.
					if err := ssr.garbageCollectDeltaSnapshots(snapList[snapStreamIndexList[snapStreamIndex]:snapStreamIndexList[snapStreamIndex+1]]); err != nil {
						continue
					}

					// Depending upon the backup mode decide which full snapshots to retain.
					switch backupMode {
					case "None":
						// backupMode "None" indicates we are processing backup in current hours. As per policy
						// we should retain all snapshots in current hour.
						deleteSnap = false
						if now.Truncate(time.Hour).Equal(snap.CreatedOn.Truncate(time.Hour)) {
							break
						}
						// Change the backupMode on first encounter of snapshot older than current hour.
						backupMode = "Hour"
						backupCount = hourModeLimit - 1
						ssr.logger.Infof("GC: Switching to Hour mode for snapshot %s", snap.CreatedOn.UTC())
						fallthrough

					case "Hour":
						// backupMode "Hour" indicates we are processing backup in current day. As per policy
						// we should retain only latest snapshots in an hour. For hour mode, we consider hours till
						// 00:00am of the same day. Instead of running algorithm for 24 hours before relative to
						// current time.
						//
						// Here it is safe to start backupCount from 23, because of the assumption mentioned abouve,
						// i.e snapList is sorted while processing.
						for backupCount >= 0 {
							rounded := time.Date(now.Year(), now.Month(), now.Day(), backupCount, 0, 0, 0, now.Location())
							diff := rounded.Sub(snap.CreatedOn.Truncate(time.Hour))
							if diff == 0 {
								// We have found the first/latest for current hour, switch to track next hour
								deleteSnap = false
								backupCount--
								if backupCount == -1 {
									ssr.logger.Infof("GC: Switching to Day mode for snapshot %s", snap.CreatedOn.UTC())
									backupMode = "Day"
									backupCount = dayModeLimit - 1
								}
								break
							} else if diff > 0 {
								// We simply decrease the count to track next hour since we don't have any snapshot
								// to process within this hour.
								backupCount--
							} else {
								// We change the backupCount once we encounter the first snapshot, so this case is hit
								// for the remainging snapshot in earlier hour, which needs to be deleted.
								deleteSnap = true
								break
							}
						}
						if backupCount >= 0 {
							break
						}
						if backupMode == "Day" {
							break
						}
						ssr.logger.Infof("GC: Switching to Day mode for snapshot %s", snap.CreatedOn.UTC())
						backupMode = "Day"
						backupCount = dayModeLimit - 1
						fallthrough

					case "Day":
						for backupCount >= 0 {
							rounded := time.Date(now.Year(), now.Month(), now.Day()-7+backupCount, 0, 0, 0, 0, now.Location())
							diff := rounded.Sub(snap.CreatedOn.Truncate(time.Hour * 24))
							if diff == 0 {
								deleteSnap = false
								backupCount--
								if backupCount == -1 {
									ssr.logger.Infof("GC: Switching to Week mode for snapshot %s", snap.CreatedOn.UTC())
									backupMode = "Week"
									backupCount = weekModeLimit - 1
								}
								break
							} else if diff > 0 {
								backupCount--
							} else {
								deleteSnap = true
								break
							}
						}
						if backupCount >= 0 {
							break
						}
						if backupMode == "Week" {
							break
						}
						ssr.logger.Infof("GC: Switching to Week mode for snapshot %s", snap.CreatedOn.UTC())
						backupMode = "Week"
						backupCount = weekModeLimit - 1
						fallthrough

					case "Week":
						for backupCount >= 0 {
							rounded := time.Date(now.Year(), now.Month(), now.Day()-dayModeLimit-7*(weekModeLimit-backupCount), 0, 0, 0, 0, now.Location())
							diff := int(rounded.Sub(snap.CreatedOn.Truncate(time.Hour)).Hours()/24) / 7
							if diff == 0 {
								deleteSnap = false
								backupCount--
								if backupCount == -1 {
									ssr.logger.Infof("GC: Switching to Month mode for snapshot %s", snap.CreatedOn.UTC())
									backupMode = "Month"
								}
								break
							} else if diff > 0 {
								backupCount--
							} else {
								deleteSnap = true
								break
							}
						}
						if backupCount >= 0 {
							break
						}
						if backupMode == "Month" {
							break
						}
						ssr.logger.Infof("GC: Switching to Month mode for snapshot %s", snap.CreatedOn.UTC())
						backupMode = "Month"
						fallthrough

					case "Month":
						deleteSnap = true

					}

					if deleteSnap {
						ssr.logger.Infof("GC: Deleting old full snapshot: %s %v", snap.CreatedOn.UTC(), deleteSnap)
						if err := ssr.store.Delete(*snap); err != nil {
							ssr.logger.Warnf("GC: Failed to delete snapshot %s: %v", path.Join(snap.SnapDir, snap.SnapName), err)
						}
					}
				}

			case GarbageCollectionPolicyLimitBased:
				// Delete delta snapshots in all snapStream but the latest one.
				// Delete all snapshots beyond limit set by ssr.maxBackups.
				for snapStreamIndex := 0; snapStreamIndex < len(snapStreamIndexList)-1; snapStreamIndex++ {
					if err := ssr.garbageCollectDeltaSnapshots(snapList[snapStreamIndexList[snapStreamIndex]:snapStreamIndexList[snapStreamIndex+1]]); err != nil {
						continue
					}
					if snapStreamIndex < len(snapStreamIndexList)-ssr.maxBackups {
						snap := snapList[snapStreamIndexList[snapStreamIndex]]
						ssr.logger.Infof("GC: Deleting old full snapshot: %s", path.Join(snap.SnapDir, snap.SnapName))
						if err := ssr.store.Delete(*snap); err != nil {
							ssr.logger.Warnf("GC: Failed to delete snapshot %s: %v", path.Join(snap.SnapDir, snap.SnapName), err)
						}
					}
				}
			}
		}
	}
}

// garbageCollectDeltaSnapshots deletes only the delta snapshots from time sorted <snapStream>. It won't delete the full snapshot
// in snapstream which supposed to be at index 0 in <snapStream>.
func (ssr *Snapshotter) garbageCollectDeltaSnapshots(snapStream snapstore.SnapList) error {
	for i := len(snapStream) - 1; i > 0; i-- {
		ssr.logger.Infof("GC: Deleting old delta snapshot: %s", path.Join(snapStream[i].SnapDir, snapStream[i].SnapName))
		if err := ssr.store.Delete(*snapStream[i]); err != nil {
			ssr.logger.Warnf("GC: Failed to delete snapshot %s: %v", path.Join(snapStream[i].SnapDir, snapStream[i].SnapName), err)
			return err
		}
	}
	return nil
}
