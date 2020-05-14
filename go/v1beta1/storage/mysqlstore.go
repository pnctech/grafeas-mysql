//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"encoding/json"

	"github.com/fernet/fernet-go"
	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes"
	"github.com/google/uuid"
	"github.com/grafeas/grafeas/go/config"
	"github.com/grafeas/grafeas/go/name"
	pb "github.com/grafeas/grafeas/proto/v1beta1/grafeas_go_proto"
	prpb "github.com/grafeas/grafeas/proto/v1beta1/project_go_proto"
	"github.com/go-sql-driver/mysql"
	"golang.org/x/net/context"
	fieldmaskpb "google.golang.org/genproto/protobuf/field_mask"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type MySQLStore struct {
	*sql.DB
	paginationKey string
}

func NewMySQLStore(config *config.MySQLConfig) (*MySQLStore, error) {
	paginationKey := config.PaginationKey
	if paginationKey == "" {
		log.Println("pagination key is empty, generating...")
		var key fernet.Key
		if err := key.Generate(); err != nil {
			return nil, errors.New(fmt.Sprintf("failed to generate pagination key, %s", err))
		}
		paginationKey = key.Encode()
	} else {
		// Validate pagination key
		_, err := fernet.DecodeKey(paginationKey)
		if err != nil {
			return nil, errors.New("invalid pagination key; must be 32-bit URL-safe base64")
		}
	}
	if err := myscreateDatabase(MySCreateSourceString(config.User, config.Password, config.Host, "mysql", config.SSLMode), config.DbName); err != nil {
		return nil, err
	}
	db, err := sql.Open("mysql", MySCreateSourceString(config.User, config.Password, config.Host, config.DbName, config.SSLMode))
	if err != nil {
		return nil, err
	}
	if db.Ping() != nil {
		return nil, errors.New("database server is not alive")
	}
    for _, query := range mysqlCreateTables {
        if _, err := db.Exec(query); err != nil {
            db.Close()
			log.Printf("error executing %s: %s", query, err)
            return nil, err
        }
    }
	log.Printf("MySQL db connection created: %v\n", db)
	return &MySQLStore{
		DB:            db,
		paginationKey: paginationKey,
	}, nil
}

func myscreateDatabase(source, dbName string) error {
	db, err := sql.Open("mysql", source)
	if err != nil {
		return err
	}
	defer db.Close()
	// Check if db exists
	res, err := db.Query(
		fmt.Sprintf("select count(*) from information_schema.schemata where schema_name = '%s'", dbName))
	if err != nil {
		return err
	} 
	if err, ok := err.(*mysql.MySQLError); ok {
		return err
	} 
	var rowCnt int
	res.Next()
	res.Scan(&rowCnt)
	if err != nil {
		return err
	}
	// Create database if it doesn't exist
	if rowCnt == 0 {
		_, err = db.Exec(fmt.Sprintf("CREATE DATABASE %s;", dbName))
		if err != nil {
			fmt.Println(err)
			return err
		}
	}
	return nil
}

// CreateProject adds the specified project to the store
func (pg *MySQLStore) CreateProject(ctx context.Context, pID string, p *prpb.Project) (*prpb.Project, error) {
	_, err := pg.DB.Exec(mysqlInsertProject, name.FormatProject(pID))
	if err != nil {
		log.Println("Failed to insert Project in database", err)
		return nil, status.Error(codes.Internal, "Failed to insert Project in database")
	}
	return p, nil
}

// DeleteProject deletes the project with the given pID from the store
func (pg *MySQLStore) DeleteProject(ctx context.Context, pID string) error {
	pName := name.FormatProject(pID)
	result, err := pg.DB.Exec(mysqlDeleteProject, pName)
	if err != nil {
		return status.Error(codes.Internal, "Failed to delete Project from database")
	}
	count, err := result.RowsAffected()
	if err != nil {
		return status.Error(codes.Internal, "Failed to delete Project from database")
	}
	if count == 0 {
		return status.Errorf(codes.NotFound, "Project with name %q does not Exist", pName)
	}
	return nil
}

// GetProject returns the project with the given pID from the store
func (pg *MySQLStore) GetProject(ctx context.Context, pID string) (*prpb.Project, error) {
	pName := name.FormatProject(pID)
	var exists bool
	err := pg.DB.QueryRow(mysqlProjectExists, pName).Scan(&exists)
	if err != nil {
		return nil, status.Error(codes.Internal, "Failed to query Project from database")
	}
	if !exists {
		return nil, status.Errorf(codes.NotFound, "Project with name %q does not Exist", pName)
	}
	return &prpb.Project{Name: pName}, nil
}

// ListProjects returns up to pageSize number of projects beginning at pageToken (or from
// start if pageToken is the empty string).
func (pg *MySQLStore) ListProjects(ctx context.Context, filter string, pageSize int, pageToken string) ([]*prpb.Project, string, error) {
	var rows *sql.Rows
	id := decryptInt64(pageToken, pg.paginationKey, 0)
    rows, err := pg.DB.Query(mysqlListProjects, id, pageSize)
	if err != nil {
		return nil, "", status.Error(codes.Internal, "Failed to list Projects from database")
	}
	count, err := pg.count(mysqlProjectCount)
	if err != nil {
		return nil, "", status.Error(codes.Internal, "Failed to count Projects from database")
	}
	var projects []*prpb.Project
	var lastId int64
	for rows.Next() {
		var name string
		err := rows.Scan(&lastId, &name)
		if err != nil {
			return nil, "", status.Error(codes.Internal, "Failed to scan Project row")
		}
		projects = append(projects, &prpb.Project{Name: name})
	}
	if count == lastId {
		return projects, "", nil
	}
	encryptedPage, err := encryptInt64(lastId, pg.paginationKey)
	if err != nil {
		return nil, "", status.Error(codes.Internal, "Failed to paginate projects")
	}
	return projects, encryptedPage, nil
}

// CreateOccurrence adds the specified occurrence
func (pg *MySQLStore) CreateOccurrence(ctx context.Context, pID, uID string, o *pb.Occurrence) (*pb.Occurrence, error) {
	o = proto.Clone(o).(*pb.Occurrence)
	o.CreateTime = ptypes.TimestampNow()

	var id string
	if nr, err := uuid.NewRandom(); err != nil {
		return nil, status.Error(codes.Internal, "Failed to generate UUID")
	} else {
		id = nr.String()
	}
	o.Name = fmt.Sprintf("projects/%s/occurrences/%s", pID, id)

	nPID, nID, err := name.ParseNote(o.NoteName)
	if err != nil {
		log.Printf("Invalid note name: %v", o.NoteName)
		return nil, status.Error(codes.InvalidArgument, "Invalid note name")
	}
	occ, err := json.Marshal(o)
    if err != nil {
		log.Println("failed to marshal note")
	}
	_, err = pg.DB.Exec(mysqlInsertOccurrence, pID, id, nPID, nID, occ)
	if err != nil {
		log.Println("Failed to insert Occurrence in database", err, occ)
		return nil, status.Error(codes.Internal, "Failed to insert Occurrence in database")
	}
	return o, nil
}

// BatchCreateOccurrence batch creates the specified occurrences in PostreSQL.
func (pg *MySQLStore) BatchCreateOccurrences(ctx context.Context, pID string, uID string, occs []*pb.Occurrence) ([]*pb.Occurrence, []error) {
	clonedOccs := []*pb.Occurrence{}
	for _, o := range occs {
		clonedOccs = append(clonedOccs, proto.Clone(o).(*pb.Occurrence))
	}
	occs = clonedOccs

	errs := []error{}
	created := []*pb.Occurrence{}
	for _, o := range occs {
		occ, err := pg.CreateOccurrence(ctx, pID, uID, o)
		if err != nil {
			// Occurrence already exists, skipping.
			continue
		} else {
			created = append(created, occ)
		}
	}

	return created, errs
}

// DeleteOccurrence deletes the occurrence with the given pID and oID
func (pg *MySQLStore) DeleteOccurrence(ctx context.Context, pID, oID string) error {
	result, err := pg.DB.Exec(mysqlDeleteOccurrence, pID, oID)
	if err != nil {
		return status.Error(codes.Internal, "Failed to delete Occurrence from database")
	}
	count, err := result.RowsAffected()
	if err != nil {
		return status.Error(codes.Internal, "Failed to delete Occurrence from database")
	}
	if count == 0 {
		return status.Errorf(codes.NotFound, "Occurrence with name %q/%q does not Exist", pID, oID)
	}
	return nil
}

// UpdateOccurrence updates the existing occurrence with the given projectID and occurrenceID
func (pg *MySQLStore) UpdateOccurrence(ctx context.Context, pID, oID string, o *pb.Occurrence, mask *fieldmaskpb.FieldMask) (*pb.Occurrence, error) {
	o = proto.Clone(o).(*pb.Occurrence)
	o.UpdateTime = ptypes.TimestampNow()

	occ, err := json.Marshal(o)
    if err != nil {
		log.Println("failed to marshal note")
	}
	result, err := pg.DB.Exec(mysqlUpdateOccurrence, occ, pID, oID)
	if err != nil {
		return nil, status.Error(codes.Internal, "Failed to update Occurrence")
	}
	count, err := result.RowsAffected()
	if err != nil {
		return nil, status.Error(codes.Internal, "Failed to update Occurrence")
	}
	if count == 0 {
		return nil, status.Errorf(codes.NotFound, "Occurrence with name %q/%q does not Exist", pID, oID)
	}
	return o, nil
}

// GetOccurrence returns the occurrence with pID and oID
func (pg *MySQLStore) GetOccurrence(ctx context.Context, pID, oID string) (*pb.Occurrence, error) {
	var data string
	err := pg.DB.QueryRow(mysqlSearchOccurrence, pID, oID).Scan(&data)
	switch {
	case err == sql.ErrNoRows:
		return nil, status.Errorf(codes.NotFound, "Occurrence with name %q/%q does not Exist", pID, oID)
	case err != nil:
		return nil, status.Error(codes.Internal, "Failed to query Occurrence from database")
	}
	var o pb.Occurrence
	json.Unmarshal([]byte(data), &o)
	if err != nil {
		return nil, status.Error(codes.Internal, "Failed to unmarshal Occurrence from database")
	}
	// Set the output-only field before returning
	o.Name = name.FormatOccurrence(pID, oID)
	return &o, nil
}

// ListOccurrences returns up to pageSize number of occurrences for this project beginning
// at pageToken, or from start if pageToken is the empty string.
func (pg *MySQLStore) ListOccurrences(ctx context.Context, pID, filter, pageToken string, pageSize int32) ([]*pb.Occurrence, string, error) {
	var rows *sql.Rows
	id := decryptInt64(pageToken, pg.paginationKey, 0)
    var filter_query, query string
    if filter != "" {
        var fs MysqlFilterSql
        filter_query = "AND " +fs.ParseFilter(filter)
    } else {
        filter_query = ""
    }
    // apply the filter to the list:
    query = fmt.Sprintf(mysqlListOccurrences, filter_query)
	rows, err := pg.DB.Query(query, pID, id, pageSize)
	if err != nil {
		return nil, "", status.Error(codes.Internal, "Failed to list Occurrences from database")
	}
    // apply the filter to the count:
    query = fmt.Sprintf(mysqlOccurrenceCount, filter_query)
	count, err := pg.count(query, pID)
	if err != nil {
		return nil, "", status.Error(codes.Internal, "Failed to count Occurrences from database")
	}
	var os []*pb.Occurrence
	var lastId int64
	for rows.Next() {
		var data string
		err := rows.Scan(&lastId, &data)
		if err != nil {
			return nil, "", status.Error(codes.Internal, "Failed to scan Occurrences row")
		}
		var o pb.Occurrence
		json.Unmarshal([]byte(data), &o)
		if err != nil {
			return nil, "", status.Error(codes.Internal, "Failed to unmarshal Occurrence from database")
		}
		os = append(os, &o)
	}
	if count == lastId {
		return os, "", nil
	}
	encryptedPage, err := encryptInt64(lastId, pg.paginationKey)
	if err != nil {
		return nil, "", status.Error(codes.Internal, "Failed to paginate projects")
	}
	return os, encryptedPage, nil
}

// CreateNote adds the specified note
func (pg *MySQLStore) CreateNote(ctx context.Context, pID, nID, uID string, n *pb.Note) (*pb.Note, error) {
	n = proto.Clone(n).(*pb.Note)
	nName := name.FormatNote(pID, nID)
	n.Name = nName
	n.CreateTime = ptypes.TimestampNow()
	note, err := json.Marshal(n)
    if err != nil {
		log.Println("failed to marshal note")
	}
	_, err = pg.DB.Exec(mysqlInsertNote, pID, nID, note)
	if err != nil {
		log.Println("Failed to insert Note in database", err)
		return nil, status.Error(codes.Internal, "Failed to insert Note in database")
	}
	return n, nil
}

// BatchCreateNotes batch creates the specified notes in memstore.
func (pg *MySQLStore) BatchCreateNotes(ctx context.Context, pID, uID string, notes map[string]*pb.Note) ([]*pb.Note, []error) {
	clonedNotes := map[string]*pb.Note{}
	for nID, n := range notes {
		clonedNotes[nID] = proto.Clone(n).(*pb.Note)
	}
	notes = clonedNotes

	errs := []error{}
	created := []*pb.Note{}
	for nID, n := range notes {
		note, err := pg.CreateNote(ctx, pID, nID, uID, n)
		if err != nil {
			// Note already exists, skipping.
			continue
		} else {
			created = append(created, note)
		}

	}

	return created, errs
}

// DeleteNote deletes the note with the given pID and nID
func (pg *MySQLStore) DeleteNote(ctx context.Context, pID, nID string) error {
	result, err := pg.DB.Exec(mysqlDeleteNote, pID, nID)
	if err != nil {
		return status.Error(codes.Internal, "Failed to delete Note from database")
	}
	count, err := result.RowsAffected()
	if err != nil {
		return status.Error(codes.Internal, "Failed to delete Note from database")
	}
	if count == 0 {
		return status.Errorf(codes.NotFound, "Note with name %q/%q does not Exist", pID, nID)
	}
	return nil
}

// UpdateNote updates the existing note with the given pID and nID
func (pg *MySQLStore) UpdateNote(ctx context.Context, pID, nID string, n *pb.Note, mask *fieldmaskpb.FieldMask) (*pb.Note, error) {
	n = proto.Clone(n).(*pb.Note)
	nName := name.FormatNote(pID, nID)
	n.Name = nName
	n.UpdateTime = ptypes.TimestampNow()

	note, err := json.Marshal(n)
    if err != nil {
		log.Println("failed to marshal note")
	}
	result, err := pg.DB.Exec(mysqlUpdateNote, note, pID, nID)
	if err != nil {
		return nil, status.Error(codes.Internal, "Failed to update Note")
	}
	count, err := result.RowsAffected()
	if err != nil {
		return nil, status.Error(codes.Internal, "Failed to update Note")
	}
	if count == 0 {
		return nil, status.Errorf(codes.NotFound, "Note with name %q/%q does not Exist", pID, nID)
	}
	return n, nil
}

// GetNote returns the note with project (pID) and note ID (nID)
func (pg *MySQLStore) GetNote(ctx context.Context, pID, nID string) (*pb.Note, error) {
	var data string
	err := pg.DB.QueryRow(mysqlSearchNote, pID, nID).Scan(&data)
	switch {
	case err == sql.ErrNoRows:
		return nil, status.Errorf(codes.NotFound, "Note with name %q/%q does not Exist", pID, nID)
	case err != nil:
		return nil, status.Error(codes.Internal, "Failed to query Note from database")
	}
	var note pb.Note
	json.Unmarshal([]byte(data), &note)
	if err != nil {
		return nil, status.Error(codes.Internal, "Failed to unmarshal Note from database")
	}
	// Set the output-only field before returning
	note.Name = name.FormatNote(pID, nID)
	return &note, nil
}

// GetOccurrenceNote gets the note for the specified occurrence from PostgreSQL.
func (pg *MySQLStore) GetOccurrenceNote(ctx context.Context, pID, oID string) (*pb.Note, error) {
	o, err := pg.GetOccurrence(ctx, pID, oID)
	if err != nil {
		return nil, err
	}
	nPID, nID, err := name.ParseNote(o.NoteName)
	if err != nil {
		log.Printf("Error parsing name: %v", o.NoteName)
		return nil, status.Error(codes.InvalidArgument, "Invalid Note name")
	}
	n, err := pg.GetNote(ctx, nPID, nID)
	if err != nil {
		return nil, err
	}
	// Set the output-only field before returning
	n.Name = name.FormatNote(nPID, nID)
	return n, nil
}

// ListNotes returns up to pageSize number of notes for this project (pID) beginning
// at pageToken (or from start if pageToken is the empty string).
func (pg *MySQLStore) ListNotes(ctx context.Context, pID, filter, pageToken string, pageSize int32) ([]*pb.Note, string, error) {
	var rows *sql.Rows
	id := decryptInt64(pageToken, pg.paginationKey, 0)
    var filter_query, query string
    if filter != "" {
        var fs MysqlFilterSql
        filter_query = "AND " +fs.ParseFilter(filter)
    } else {
        filter_query = ""
    }
    // apply the filter to the list
    query = fmt.Sprintf(mysqlListNotes, filter_query)
	rows, err := pg.DB.Query(query, pID, id, pageSize)
	if err != nil {
		return nil, "", status.Error(codes.Internal, "Failed to list Notes from database")
	}
    // apply the filter to the count
    query = fmt.Sprintf(mysqlNoteCount, filter_query)
	count, err := pg.count(query, pID)
	if err != nil {
		return nil, "", status.Error(codes.Internal, "Failed to count Notes from database")
	}
	var ns []*pb.Note
	var lastId int64
	for rows.Next() {
		var data string
		err := rows.Scan(&lastId, &data)
		if err != nil {
			return nil, "", status.Error(codes.Internal, "Failed to scan Notes row")
		}
		var n pb.Note
		json.Unmarshal([]byte(data), &n)
		if err != nil {
			return nil, "", status.Error(codes.Internal, "Failed to unmarshal Note from database")
		}
		ns = append(ns, &n)
	}
	if count == lastId {
		return ns, "", nil
	}
	encryptedPage, err := encryptInt64(lastId, pg.paginationKey)
	if err != nil {
		return nil, "", status.Error(codes.Internal, "Failed to paginate projects")
	}
	return ns, encryptedPage, nil
}

// ListNoteOccurrences returns up to pageSize number of occcurrences on the particular note (nID)
// for this project (pID) projects beginning at pageToken (or from start if pageToken is the empty string).
func (pg *MySQLStore) ListNoteOccurrences(ctx context.Context, pID, nID, filter, pageToken string, pageSize int32) ([]*pb.Occurrence, string, error) {
	// Verify that note exists
	if _, err := pg.GetNote(ctx, pID, nID); err != nil {
		return nil, "", err
	}
	var rows *sql.Rows
	id := decryptInt64(pageToken, pg.paginationKey, 0)
    var filter_query, query string
    if filter != "" {
        var fs MysqlFilterSql
        filter_query = "AND " +fs.ParseFilter(filter)
    } else {
        query = ""
    }
    query = fmt.Sprintf(mysqlListNoteOccurrences, filter_query)
	rows, err := pg.DB.Query(query, pID, nID, id, pageSize)
	if err != nil {
		return nil, "", status.Error(codes.Internal, "Failed to list Occurrences from database")
	}
    query = fmt.Sprintf(mysqlNoteOccurrencesCount, filter_query)
	count, err := pg.count(query, pID, nID)
	if err != nil {
		return nil, "", status.Error(codes.Internal, "Failed to count Occurrences from database")
	}
	var os []*pb.Occurrence
	var lastId int64
	for rows.Next() {
		var data string
		err := rows.Scan(&lastId, &data)
		if err != nil {
			return nil, "", status.Error(codes.Internal, "Failed to scan Occurrences row")
		}
		var o pb.Occurrence
		json.Unmarshal([]byte(data), &o)
		if err != nil {
			return nil, "", status.Error(codes.Internal, "Failed to unmarshal Occurrence from database")
		}
		os = append(os, &o)
	}
	if count == lastId {
		return os, "", nil
	}
	encryptedPage, err := encryptInt64(lastId, pg.paginationKey)
	if err != nil {
		return nil, "", status.Error(codes.Internal, "Failed to paginate projects")
	}
	return os, encryptedPage, nil
}

// GetVulnerabilityOccurrencesSummary gets a summary of vulnerability occurrences from storage.
func (pg *MySQLStore) GetVulnerabilityOccurrencesSummary(ctx context.Context, projectID, filter string) (*pb.VulnerabilityOccurrencesSummary, error) {
	return &pb.VulnerabilityOccurrencesSummary{}, nil
}

// CreateSourceString generates DB source path.
// username:password@protocol(address)/dbname?param=value
// %s:%s@tcp(%s)/%s
func MySCreateSourceString(user, password, host, dbName, SSLMode string) string {
	if user == "" {
		return fmt.Sprintf("tcp(%s)/%s",host, dbName)
	}
	return fmt.Sprintf("%s:%s@tcp(%s)/%s", user, password, host, dbName)
}

// count returns the total number of entries for the specified query (assuming SELECT(*) is used)
func (pg *MySQLStore) count(query string, args ...interface{}) (int64, error) {
	row := pg.DB.QueryRow(query, args...)
	var count int64
	err := row.Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, err
}


