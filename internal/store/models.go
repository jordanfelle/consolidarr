package store

import (
	"encoding/json"
	"time"
)

type Instance struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	APIKey   string `json:"apiKey,omitempty"`
	PathFrom string `json:"pathFrom"` // Sonarr's view of the download path
	PathTo   string `json:"pathTo"`   // where consolidarr sees the same files (for ffprobe)
	Enabled  bool   `json:"enabled"`
}

func (s *Store) Instances() ([]Instance, error) {
	rows, err := s.Query(`select id,name,url,api_key,path_from,path_to,enabled from instances order by id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Instance{}
	for rows.Next() {
		var i Instance
		var en int
		if err := rows.Scan(&i.ID, &i.Name, &i.URL, &i.APIKey, &i.PathFrom, &i.PathTo, &en); err != nil {
			return nil, err
		}
		i.Enabled = en == 1
		out = append(out, i)
	}
	return out, rows.Err()
}

func (s *Store) Instance(id int64) (Instance, error) {
	all, err := s.Instances()
	if err != nil {
		return Instance{}, err
	}
	for _, i := range all {
		if i.ID == id {
			return i, nil
		}
	}
	return Instance{}, ErrNotFound
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Store) SaveInstance(i Instance) (int64, error) {
	if i.ID == 0 {
		err := s.QueryRow(`insert into instances(name,url,api_key,path_from,path_to,enabled) values(?,?,?,?,?,?) returning id`,
			i.Name, i.URL, i.APIKey, i.PathFrom, i.PathTo, b2i(i.Enabled)).Scan(&i.ID)
		return i.ID, err
	}
	if i.APIKey == "" { // keep the stored key when the UI doesn't resend it
		old, err := s.Instance(i.ID)
		if err != nil {
			return 0, err
		}
		i.APIKey = old.APIKey
	}
	_, err := s.Exec(`update instances set name=?,url=?,api_key=?,path_from=?,path_to=?,enabled=? where id=?`,
		i.Name, i.URL, i.APIKey, i.PathFrom, i.PathTo, b2i(i.Enabled), i.ID)
	return i.ID, err
}

func (s *Store) DeleteInstance(id int64) error {
	for _, t := range []string{"seasons", "jobs"} {
		if _, err := s.Exec(`delete from `+t+` where inst_id=?`, id); err != nil {
			return err
		}
	}
	_, err := s.Exec(`delete from instances where id=?`, id)
	return err
}

type Job struct {
	ID         int64   `json:"id"`
	InstID     int64   `json:"instId"`
	SeriesID   int     `json:"seriesId"`
	Season     int     `json:"season"`
	Title      string  `json:"title"`
	OldFileIDs []int   `json:"oldFileIds"`
	Release    string  `json:"release"`
	Indexer    string  `json:"indexer"`
	SizeGB     float64 `json:"sizeGB"`
	Status     string  `json:"status"` // downloading ready holding importing done lost failed
	Message    string  `json:"message"`
	Progress   int     `json:"progress"`
	ImportCmd  int     `json:"importCmd"`
	Created    int64   `json:"created"`
	Updated    int64   `json:"updated"`
}

const jobCols = `id,inst_id,series_id,season,title,old_file_ids,release,indexer,size_gb,status,message,progress,import_cmd,created,updated`

func scanJob(sc interface{ Scan(...any) error }) (Job, error) {
	var j Job
	var old string
	err := sc.Scan(&j.ID, &j.InstID, &j.SeriesID, &j.Season, &j.Title, &old, &j.Release, &j.Indexer, &j.SizeGB,
		&j.Status, &j.Message, &j.Progress, &j.ImportCmd, &j.Created, &j.Updated)
	_ = json.Unmarshal([]byte(old), &j.OldFileIDs)
	return j, err
}

// Jobs lists jobs; active=true returns only unfinished ones.
func (s *Store) Jobs(active bool, limit int) ([]Job, error) {
	q := `select ` + jobCols + ` from jobs`
	if active {
		q += ` where status in ('downloading','ready','holding','importing')`
	}
	rows, err := s.Query(q+` order by id desc limit ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) AddJob(j Job) (int64, error) {
	old, _ := json.Marshal(j.OldFileIDs)
	now := time.Now().Unix()
	err := s.QueryRow(`insert into jobs(inst_id,series_id,season,title,old_file_ids,release,indexer,size_gb,status,message,progress,import_cmd,created,updated)
		values(?,?,?,?,?,?,?,?,?,?,?,?,?,?) returning id`,
		j.InstID, j.SeriesID, j.Season, j.Title, string(old), j.Release, j.Indexer, j.SizeGB, "downloading", "", 0, 0, now, now).Scan(&j.ID)
	return j.ID, err
}

func (s *Store) UpdateJob(j Job) error {
	_, err := s.Exec(`update jobs set status=?,message=?,progress=?,import_cmd=?,updated=? where id=?`,
		j.Status, j.Message, j.Progress, j.ImportCmd, time.Now().Unix(), j.ID)
	return err
}

func (s *Store) DeleteJob(id int64) error {
	_, err := s.Exec(`delete from jobs where id=?`, id)
	return err
}
