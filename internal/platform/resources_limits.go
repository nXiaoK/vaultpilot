package platform

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// CheckCacheBudget checks actual cache files. It never deletes files because
// concurrent Restic processes may be using them. Engines must check this at
// task admission and bounded work stages, and report the failure to the user.
func (r *Runtime) CheckCacheBudget() error {
	limit := r.Config.CacheMaxBytes
	if limit <= 0 {
		return errors.New("缓存容量上限未配置")
	}
	var total int64
	return filepath.WalkDir(r.Config.CacheDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) && path != r.Config.CacheDir {
				return nil
			}
			return fmt.Errorf("无法核对引擎缓存占用：%w", walkErr)
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("无法核对引擎缓存占用：%w", err)
		}
		size := info.Size()
		if size < 0 || size > math.MaxInt64-total {
			return errors.New("引擎缓存占用超出可计算范围")
		}
		total += size
		if total > limit {
			return fmt.Errorf("引擎缓存已超过上限（已检查 %d Bytes，上限 %d Bytes）；请在任务停止后清理不再使用的缓存或提高 VP_CACHE_MAX_BYTES，系统不会删除正在使用的缓存", total, limit)
		}
		return nil
	})
}

func parseMaintenanceWindow(window string) (int, int, error) {
	if window == "" {
		return 0, 0, nil
	}
	invalid := errors.New("维护窗口必须采用 HH:MM-HH:MM 格式且起止不同；留空表示不限制")
	parts := strings.Split(window, "-")
	if len(parts) != 2 {
		return 0, 0, invalid
	}
	minutes := func(value string) (int, bool) {
		if len(value) != 5 || value[2] != ':' {
			return 0, false
		}
		for _, i := range []int{0, 1, 3, 4} {
			if value[i] < '0' || value[i] > '9' {
				return 0, false
			}
		}
		h, e := strconv.Atoi(value[:2])
		if e != nil || h > 23 {
			return 0, false
		}
		m, e := strconv.Atoi(value[3:])
		if e != nil || m > 59 {
			return 0, false
		}
		return h*60 + m, true
	}
	start, ok := minutes(parts[0])
	if !ok {
		return 0, 0, invalid
	}
	end, ok := minutes(parts[1])
	if !ok || start == end {
		return 0, 0, invalid
	}
	return start, end, nil
}

// AllowedMaintenance uses the configured civil timezone, with an inclusive
// start and exclusive end. Windows crossing midnight are supported.
func (r *Runtime) AllowedMaintenance(at time.Time) (bool, error) {
	zone, err := time.LoadLocation(r.Config.MaintenanceTimezone)
	if err != nil {
		return false, errors.New("维护窗口时区无效")
	}
	start, end, err := parseMaintenanceWindow(r.Config.MaintenanceWindow)
	if err != nil {
		return false, err
	}
	if r.Config.MaintenanceWindow == "" {
		return true, nil
	}
	local := at.In(zone)
	minute := local.Hour()*60 + local.Minute()
	if start < end {
		return minute >= start && minute < end, nil
	}
	return minute >= start || minute < end, nil
}
