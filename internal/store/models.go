package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
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
	ID          int64   `json:"id"`
	InstID      int64   `json:"instId"`
	SeriesID    int     `json:"seriesId"`
	Season      int     `json:"season"`
	Title       string  `json:"title"`
	OldFileIDs  []int   `json:"oldFileIds"`
	Release     string  `json:"release"`
	Indexer     string  `json:"indexer"`
	SizeGB      float64 `json:"sizeGB"`
	Status      string  `json:"status"` // downloading ready holding importing done lost failed
	Message     string  `json:"message"`
	Progress    int     `json:"progress"`
	ImportCmd   int     `json:"importCmd"`
	Created     int64   `json:"created"`
	Updated     int64   `json:"updated"`
	Deleted     int64   `json:"deleted"`     // unix time the old files began to be deleted; 0 = never (nothing was touched)
	DownloadID  string  `json:"downloadId"`  // the download client hash this job owns
	Expected    int     `json:"expected"`    // how many episodes the pack must supply for the season to count as replaced
	ExpectedEps []int   `json:"expectedEps"` // exactly which episode numbers; done means each has a new file
	ImportAt    int64   `json:"importAt"`    // when the current import attempt started (resets on retry)
}

const jobCols = `id,inst_id,series_id,season,title,old_file_ids,release,indexer,size_gb,status,message,progress,import_cmd,created,updated,deleted,download_id,expected,expected_eps,import_at`

func scanJob(sc interface{ Scan(...any) error }) (Job, error) {
	var j Job
	var old, eps string
	err := sc.Scan(&j.ID, &j.InstID, &j.SeriesID, &j.Season, &j.Title, &old, &j.Release, &j.Indexer, &j.SizeGB,
		&j.Status, &j.Message, &j.Progress, &j.ImportCmd, &j.Created, &j.Updated, &j.Deleted, &j.DownloadID, &j.Expected, &eps, &j.ImportAt)
	_ = json.Unmarshal([]byte(old), &j.OldFileIDs)
	_ = json.Unmarshal([]byte(eps), &j.ExpectedEps)
	return j, err
}

// Jobs lists jobs; active=true returns only unfinished ones.
func (s *Store) Jobs(active bool, limit int) ([]Job, error) {
	q := `select ` + jobCols + ` from jobs`
	if active {
		q += ` where status in ('reserving','downloading','ready','holding','importing')`
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

// ErrDuplicate means the season already has an unfinished job.
var ErrDuplicate = errors.New("this season already has an active replacement")

// activeStatuses are the job states that still own their season.
const activeStatuses = `('reserving','downloading','ready','holding','importing')`

// Job returns one job by id.
func (s *Store) Job(id int64) (Job, error) {
	j, err := scanJob(s.QueryRow(`select `+jobCols+` from jobs where id=?`, id))
	if err == sql.ErrNoRows {
		return Job{}, ErrNotFound
	}
	return j, err
}

// ActiveJob reports whether the season already has an unfinished job.
func (s *Store) ActiveJob(instID int64, seriesID, season int) bool {
	var n int
	_ = s.QueryRow(`select count(*) from jobs where inst_id=? and series_id=? and season=? and status in `+activeStatuses, instID, seriesID, season).Scan(&n)
	return n > 0
}

// AddJob records a new job. A season can have only one unfinished job, so a double click, a second grab or an
// adopt racing the scheduler cannot create two jobs that would both delete the same files.
func (s *Store) AddJob(j Job) (int64, error) {
	var n int
	if err := s.QueryRow(`select count(*) from jobs where inst_id=? and series_id=? and season=? and status in `+activeStatuses,
		j.InstID, j.SeriesID, j.Season).Scan(&n); err != nil {
		return 0, err
	}
	if n > 0 {
		return 0, ErrDuplicate
	}
	old, _ := json.Marshal(j.OldFileIDs)
	now := time.Now().Unix()
	status := j.Status
	if status == "" {
		status = "downloading"
	}
	err := s.QueryRow(`insert into jobs(inst_id,series_id,season,title,old_file_ids,release,indexer,size_gb,status,message,progress,import_cmd,created,updated,download_id)
		values(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) returning id`,
		j.InstID, j.SeriesID, j.Season, j.Title, string(old), j.Release, j.Indexer, j.SizeGB, status, "", 0, 0, now, now, j.DownloadID).Scan(&j.ID)
	if err != nil && isUniqueViolation(err) {
		return 0, ErrDuplicate // lost a race to another caller that created the season's job first
	}
	return j.ID, err
}

// UpdateJob saves a job's mutable state.
func (s *Store) UpdateJob(j Job) error {
	eps, _ := json.Marshal(j.ExpectedEps)
	if j.ExpectedEps == nil {
		eps = []byte("[]")
	}
	_, err := s.Exec(`update jobs set status=?,message=?,progress=?,import_cmd=?,deleted=?,download_id=?,expected=?,expected_eps=?,import_at=?,updated=? where id=?`,
		j.Status, j.Message, j.Progress, j.ImportCmd, j.Deleted, j.DownloadID, j.Expected, string(eps), j.ImportAt, time.Now().Unix(), j.ID)
	return err
}

func (s *Store) DeleteJob(id int64) error {
	_, err := s.Exec(`delete from jobs where id=?`, id)
	return err
}

func isUniqueViolation(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "unique constraint") || strings.Contains(m, "duplicate key") || strings.Contains(m, "sqlstate 23505")
}

// ErrSetupDone means the first user already exists.
var ErrSetupDone = errors.New("already set up")

// CreateFirstUser inserts the initial account only if no user exists, in one statement, so two concurrent
// setup calls cannot both win.
func (s *Store) CreateFirstUser(username, hash string) (int64, error) {
	var id int64
	err := s.QueryRow(`insert into users(username,pw_hash) select ?,? where not exists (select 1 from users) returning id`, username, hash).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrSetupDone
	}
	return id, err
}
