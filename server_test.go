package main

import (
	"encoding/json"
	"errors"
	"flag"
	server3 "github.com/fclairamb/ftpserver/server"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GeertJohan/go.rice"
	"github.com/artpar/go-guerrilla"
	server2 "github.com/artpar/go-imap/server"
	"github.com/daptin/daptin/server"
	"github.com/daptin/daptin/server/resource"
	"github.com/daptin/daptin/server/statementbuilder"
	"github.com/gin-gonic/gin"
	"github.com/gocraft/health"
	"github.com/imroc/req"
	"github.com/jamiealquiza/envy"
	"github.com/jmoiron/sqlx"
	"github.com/sadlil/go-trigger"
)

const testData = `{
  "cloud_store": [
    {
      "name": "local-store",
      "store_type": "local",
      "store_provider": "local",
      "root_path": "${rootPath}gallery",
      "store_parameters": "{}",
      "reference_id": "ca122915-4dbb-42cf-aa19-c89a14e6fa9a"
    }
  ],
  "site": [
    {
      "name": "gallery",
      "hostname": "gallery.daptin.com",
      "path": "gallery",
      "cloud_store_id": "ca122915-4dbb-42cf-aa19-c89a14e6fa9a"
    }
  ]
}`

const testSchemas = `Tables:
  - TableName: gallery_image
    Columns:
      - Name: title
        DataType: varchar(500)
        ColumnType: label
      - Name: file
        DataType: text
        IsNullable: true
        ColumnType: image.png|jpg|jpeg|gif|tiff
        IsForeignKey: true
        ForeignKeyData:
          DataSource: cloud_store
          Namespace: local-store
          KeyName: images
Imports:
  - FilePath: initial_data.json
    Entity: site
    FileType: json`

func TestServer(t *testing.T) {

	dir := os.TempDir()
	if dir[len(dir)-1] != os.PathSeparator {
		dir = dir + string(os.PathSeparator)
	}
	tempDir := dir + "daptintest" + string(os.PathSeparator)
	t.Logf("Test directory: %v", dir)

	m := make(map[string]interface{})
	err := json.Unmarshal([]byte(testData), &m)
	t.Errorf("Err: %v", err)

	schema := strings.Replace(testSchemas, "${imagePath}", tempDir, -1)
	schema = strings.Replace(schema, "${rootPath}", tempDir, -1)
	data := strings.Replace(testData, "${rootPath}", tempDir, -1)
	_ = os.Mkdir(tempDir, 0777)


	err = json.Unmarshal([]byte(data), &m)
	t.Errorf("Err: %v", err)

	_ = os.Mkdir(tempDir+string(os.PathSeparator)+"gallery", 0777)
	_ = os.Mkdir(tempDir+string(os.PathSeparator)+"gallery"+string(os.PathSeparator)+"images", 0777)

	_ = ioutil.WriteFile(tempDir+string(os.PathSeparator)+"schema_test_daptin.yaml", []byte(schema), os.ModePerm)
	_ = ioutil.WriteFile(tempDir+string(os.PathSeparator)+"initial_data.json", []byte(data), os.ModePerm)

	_ = os.Setenv("DAPTIN_SCHEMA_FOLDER", tempDir)

	_ = os.Remove("daptin_test.db")

	var dbType = flag.String("db_type", "sqlite3", "Database to use: sqlite3/mysql/postgres")
	var connectionString = flag.String("db_connection_string", "daptin_test.db", "\n\tSQLite: test.db\n"+
		"\tMySql: <username>:<password>@tcp(<hostname>:<port>)/<db_name>\n"+
		"\tPostgres: host=<hostname> port=<port> user=<username> password=<password> dbname=<db_name> sslmode=enable/disable")

	var webDashboardSource = flag.String("dashboard", "daptinweb/dist", "path to dist folder for daptin web dashboard")
	//var assetsSource = flag.String("assets", "assets", "path to folder for assets")
	var port = flag.String("port", ":6337", "Daptin port")
	var runtimeMode = flag.String("runtime", "release", "Runtime for Gin: debug, test, release")

	gin.SetMode(*runtimeMode)

	envy.Parse("DAPTIN") // looks for DAPTIN_PORT, DAPTIN_DASHBOARD, DAPTIN_DB_TYPE, DAPTIN_RUNTIME
	flag.Parse()

	stream.AddSink(&health.WriterSink{
		Writer: os.Stdout,
	})
	boxRoot1, err := rice.FindBox("daptinweb/dist/")

	var boxRoot http.FileSystem
	if err != nil {
		log.Printf("Try loading web dashboard from: %v", *webDashboardSource)
		boxRoot = http.Dir(*webDashboardSource)
	} else {
		boxRoot = boxRoot1.HTTPBox()
	}
	statementbuilder.InitialiseStatementBuilder(*dbType)

	db, err := server.GetDbConnection(*dbType, *connectionString)
	if err != nil {
		panic(err)
	}
	log.Printf("Connection acquired from database")

	var hostSwitch server.HostSwitch
	var mailDaemon *guerrilla.Daemon
	var taskScheduler resource.TaskScheduler
	var configStore *resource.ConfigStore
	var certManager *resource.CertificateManager
	var imapServer *server2.Server
	var ftpServer *server3.FtpServer

	configStore, _ = resource.NewConfigStore(db)
	configStore.SetConfigValueFor("graphql.enable", "true", "backend")
	configStore.SetConfigValueFor("imap.enabled", "true", "backend")
	configStore.SetConfigValueFor("imap.listen_interface", ":8743", "backend")
	configStore.SetConfigValueFor("logs.enable", "true", "backend")

	hostSwitch, mailDaemon, taskScheduler, configStore, certManager, ftpServer, imapServer = server.Main(boxRoot, db)

	rhs := TestRestartHandlerServer{
		HostSwitch: &hostSwitch,
	}

	trigger.On("restart", func() {
		log.Printf("Trigger restart")

		taskScheduler.StartTasks()
		mailDaemon.Shutdown()
		err = db.Close()
		if err != nil {
			log.Printf("Failed to close DB connections: %v", err)
		}

		db, err = server.GetDbConnection(*dbType, *connectionString)

		hostSwitch, mailDaemon, taskScheduler, configStore, certManager, ftpServer, imapServer = server.Main(boxRoot, db)
		rhs.HostSwitch = &hostSwitch
	})

	name, _ := os.Hostname()
	certManager.GetTLSConfig(name, true)

	log.Printf("Listening at port: %v", *port)

	srv := &http.Server{Addr: *port, Handler: rhs.HostSwitch}

	go func() {
		srv.ListenAndServe()
	}()
	time.Sleep(2 * time.Second)

	err = RunTests(t, hostSwitch, mailDaemon, db, taskScheduler, configStore)
	if err != nil {
		t.Errorf("test failed %v", err)
	}
	log.Printf("it never started in test: %v %v", imapServer, ftpServer)

	log.Printf("Shutdown now")

}

func RunTests(t *testing.T, hostSwitch server.HostSwitch, daemon *guerrilla.Daemon, db *sqlx.DB, scheduler resource.TaskScheduler, configStore *resource.ConfigStore) error {

	const baseAddress = "http://localhost:6337"

	r := req.New()

	responseMap := make(map[string]interface{})

	resp, err := r.Get(baseAddress + "/api/world")
	if err != nil {
		log.Printf("Failed to get %s %s", "world", err)
		return err
	}

	resp.ToJSON(&responseMap)

	data := responseMap["data"].([]interface{})
	firstRow := data[0].(map[string]interface{})

	if firstRow["type"] != "world" {
		t.Errorf("world type mismatch")
	}

	resp, err = r.Get(baseAddress + "/actions")

	if err != nil {
		return err
	}

	actionMap := make(map[string]interface{})
	resp.ToJSON(&actionMap)

	signInAction, ok := actionMap["user:signin"].(map[string]interface{})
	if !ok {
		t.Errorf("signin action not found")
	}

	if signInAction["OnType"] != "user_account" {
		t.Errorf("Unexpected on type")
	}

	signUpAction, ok := actionMap["user:signup"].(map[string]interface{})
	if !ok {
		t.Errorf("signin action not found")
	}
	if signUpAction["OnType"] != "user_account" {
		t.Errorf("Unexpected on type")
	}

	resp, err = r.Get(baseAddress + "/meta?query=column_types")
	if err != nil {
		log.Printf("Failed to get %s %s", "meta", err)
		return err
	}

	cols := make(map[string]interface{})
	resp.ToJSON(&cols)

	if cols["label"] == nil {
		t.Errorf("label not found")
	}

	resp, err = r.Post(baseAddress+"/action/user_account/signup", req.BodyJSON(map[string]interface{}{
		"attributes": map[string]interface{}{
			"email":           "test@gmail.com",
			"name":            "name",
			"password":        "tester123",
			"passwordConfirm": "tester123",
		},
	}))

	if err != nil {
		return err
	}
	var signUpResponse interface{}

	resp.ToJSON(&signUpResponse)

	if signUpResponse.([]interface{})[0].(map[string]interface{})["ResponseType"] != "client.notify" {
		t.Errorf("Unexpected response type from sign up")
	}

	resp, err = r.Post(baseAddress+"/action/user_account/signin", req.BodyJSON(map[string]interface{}{
		"attributes": map[string]interface{}{
			"email":    "test@gmail.com",
			"password": "tester123",
		},
	}))

	if err != nil {
		return err
	}

	var token string
	var signInResponse interface{}

	resp.ToJSON(&signInResponse)

	responseAttr := signInResponse.([]interface{})[0].(map[string]interface{})
	if responseAttr["ResponseType"] != "client.store.set" {
		t.Errorf("Unexpected response type from sign up")
	}

	token = responseAttr["Attributes"].(map[string]interface{})["value"].(string)

	//t.Logf("Token: %v", token)

	resp, err = r.Get(baseAddress + "/recline_model")
	if err != nil {
		log.Printf("Failed to get %s %s", "recline_model", err)
		return err
	}
	reclineModelMap := make(map[string]interface{})
	err = resp.ToJSON(&reclineModelMap)
	if err != nil {
		log.Printf("Failed to get %s %s", "json recline model", err)
		return err
	}

	if reclineModelMap["alias"] != "string" {
		return errors.New("unexpected recline model response")
	}

	resp, err = r.Get(baseAddress + "/jsmodel/world.js")
	if err != nil {
		log.Printf("Failed to get %s %s", "jsmodel world", err)
		return err
	}
	jsModelMap := make(map[string]interface{})
	err = resp.ToJSON(&jsModelMap)
	if err != nil {
		log.Printf("Failed to get %s %s", "unmarshal jsmomdel world", err)
		return err
	}

	if jsModelMap["ColumnModel"] == nil {
		return errors.New("unexpected model map response")
	}

	_, err = r.Get(baseAddress + "/favicon.ico")
	if err != nil {
		log.Printf("Failed to get %s %s", "favicon.ico", err)
		return err
	}

	_, err = r.Get(baseAddress + "/favicon.png")
	if err != nil {
		log.Printf("Failed to get %s %s", "favicon.png", err)
		return err
	}

	resp, err = r.Get(baseAddress + "/statistics")
	if err != nil {
		log.Printf("Failed to get %s %s", "statistics", err)
		return err
	}

	resp, err = r.Get(baseAddress + "/openapi.yaml")
	if err != nil {
		log.Printf("Failed to get %s %s", "openapi.yaml", err)
		return err
	}

	// check user flow
	resp, err = r.Get(baseAddress+"/api/world", req.Header{
		"Authorization": "Bearer " + token,
	})
	if err != nil {
		log.Printf("Failed to get %s %s", "world with token ", err)
		return err
	}

	resp.ToJSON(&responseMap)

	data = responseMap["data"].([]interface{})
	firstRow = data[0].(map[string]interface{})

	if firstRow["type"] != "world" {
		t.Errorf("world type mismatch")
	}

	resp, err = r.Get(baseAddress+"/api/gallery_image?sort=reference_id,-created_at", req.Header{
		"Authorization": "Bearer " + token,
	})

	if err != nil {
		log.Printf("Failed to get %s %s", "gallerty image get", err)
		return err
	}

	resp, err = r.Post(baseAddress+"/api/gallery_image", req.BodyJSON(OneImage))
	if err != nil {
		log.Printf("Failed to get %s %s", "gallery image post", err)
		return err
	}

	createImageResp := make(map[string]interface{})
	err = resp.ToJSON(&createImageResp)
	if err != nil {
		log.Printf("Failed to get %s %s", "unmarshal gallery image post", err)
		return err
	}

	var createdID string
	createdID = createImageResp["data"].(map[string]interface{})["attributes"].(map[string]interface{})["reference_id"].(string)

	t.Logf("Image create response id: %v", createdID)

	resp, err = r.Get(baseAddress + "/api/gallery_image/" + createdID)
	readImageResp := make(map[string]interface{})
	err = resp.ToJSON(&readImageResp)
	if err != nil {
		log.Printf("Failed to get %s %s", "unmarshal gallery image get", err)
		return err
	}

	//t.Logf("Image read response id: %v", readImageResp)

	resp, err = r.Get(baseAddress + "/asset/gallery_image/" + createdID + "/file.png")
	if err != nil {
		log.Printf("Failed to get %s %s", "gallery image get by id", err)
		return err
	}

	imbBody, err := ioutil.ReadAll(resp.Response().Body)
	if err != nil {
		log.Printf("Failed to get %s %s", "read image body gallery image get by id", err)
		return err
	}
	imgLen := len(imbBody)
	t.Logf("Image length: %v", imgLen)

	if imgLen == 0 {
		t.Errorf("Image length is 0")
	}

	Params := []string{
		"boxblur=0.5",
		"gaussianblur=0.5",
		"dilate=0.5",
		"edgedetection=0.5",
		"erode=0.5",
		"emboss=0.5",
		"median=0.5",
		"sharpen=0.5",
		"brightness=0.5",
		"colorBalance=0.5,0.5,0.5",
		"colorize=0.5,0.5,0.5",
		"colorspaceLinearToSRGB=0.5",
		"colorspaceSRGBToLinear=0.5",
		"contrast=0.5",
		"crop=10,15,20,30",
		"cropToSize=10,20,CenterAnchor",
		"flipHorizontal=1",
		"flipVertical=1",
		"gamma=0.6",
		"gaussianBlur=0.6",
		"grayscale=true",
		"hue=0.6",
		"invert=true",
		"resize=10,40,NearestNeighbor",
		"resize=10,40,Box",
		"resize=10,40,Linear",
		"resize=10,40,Cubic",
		"resize=10,40,Lanczos",
		"rotate=0.5,EWD,NearestNeighborInterpolation",
		"rotate180=true",
		"rotate270=true",
		"rotate90=true",
		"saturation=0.6",
		"sepia=0.6",
		"sobel=true",
		"threshold=0.6",
		"transpose=true",
		"transverse=true",
	}

	for _, param := range Params {
		resp, err = r.Get(baseAddress + "/asset/gallery_image/" + createdID + "/file.png?" + param)
		if err != nil {
			log.Printf("Failed to get %s %s", param, err)
			return err
		}

		imbBody, err := ioutil.ReadAll(resp.Response().Body)
		if err != nil {
			log.Printf("Failed to get read image %s %s", param, err)
			return err
		}
		t.Logf("Image length [%v]: %v", param, len(imbBody))

	}

	// do a sign in
	resp, err = r.Post(baseAddress+"/action/world/become_admin", req.BodyJSON(map[string]interface{}{
		"attributes": map[string]interface{}{},
	}), req.Header{
		"Authorization": "Bearer " + token,
	})

	if err != nil {
		log.Printf("Failed to get read image %s %s", "become admin", err)
		return err
	}

	becomeAdminResponse := resp.String()
	t.Logf("Become admin response: [%v]", becomeAdminResponse)

	resp, err = r.Get(baseAddress+"/_config/backend/hostname", req.Header{
		"Authorization": "Bearer " + token,
	})
	if err != nil {
		log.Printf("Failed to get read image %s %s", "config hostname get", err)
		return err
	}

	t.Logf("Hostname from config: %v", resp.String())

	resp, err = r.Post(baseAddress+"/_config/backend/hostname", req.Header{
		"Authorization": "Bearer " + token,
	}, "test")
	if err != nil {
		log.Printf("Failed to get read image %s %s", "config hostname post", err)
		return err
	}

	t.Logf("Hostname from config: %v", resp.String())

	return nil

}

type TestRestartHandlerServer struct {
	HostSwitch *server.HostSwitch
}

func (rhs *TestRestartHandlerServer) ServeHTTP(rew http.ResponseWriter, req *http.Request) {
	rhs.HostSwitch.ServeHTTP(rew, req)
}

const OneImage = `{"data":{"type":"gallery_image","attributes":{"file":[{"name":"image.png","file":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAqAAAAChCAYAAAAY/w1JAAAABHNCSVQICAgIfAhkiAAAABl0RVh0U29mdHdhcmUAZ25vbWUtc2NyZWVuc2hvdO8Dvz4AACAASURBVHic7d1xcFT1/e//5/d7m917haxodx1lGW0CtllwSNS60U6WX3VDkYB8WbST8BUDIglSgheTr8qCQkQgCCZyS6gNsYFEWpLpV9YqBCkJdrL53TbbWhNHWaoQCsOq466IG8O9u3x/098fuwkBEggKi6mvxwwzwtl8zvucPTnndT6fzzn+y7HAR/9ARERERCRB/vVKFyAiIiIi3y4KoCIiIiKSUAqgIiIiIpJQCqAiIiIiklAKoCIiIiKSUAqgIiIiIpJQCqAiIiIiklAKoCIiIiKSUAqgIiIiIpJQCqAiIiIiklAKoCIiIiKSUAqgIiIiIpJQCqAiIiIiklAKoCIiIiKSUAqgIiIiIpJQCqAiIiIiklAKoCIiIiKSUAqgIiIiIpJQCqAiIiIiklDfudQNmstWX+omRUREROQbIORedknaueQBFOD/lq27HM2KiIiIyBXy391PXrK2NAQvIiIiIgmlACoiIiIiCaUAKiIiIiIJpQAqIiIiIgmlACoiIiIiCaUAKiIiIiIJpQAqIiIiIgmlACoiIiIiCaUAKiIiIiIJpQAqIiIiIgmlACoiIiIiCaUAKiIiIiIJpQAqIiIiIgmlACoiIiIiCaUAKiIiIiIJ9e0LoJ0eFk+3My4tnduKm4h83fYijSwYb+PmrFLaL0V930Y+N3el2RhX3HSlK/laIk1FjEuzcdcK3yA+HWR7ro2b01xUdV720i5CJ1XT03mwPnhpmmsqZlxWKW2XprXLZ6jUKSLyT2JoBNCwhwfTbNzc+yedcVlOHlhYxqvt4Ytqqq26gl0HurA4i3jCZcN4yYuN0LbCwbjces53CY8EvFS585mUZWdcmo1xdziZNq+MV/0Xtz1DUrCeB8c7eKonp1lzmF8wj8dyUi/fOpuKGRc/fn685sxbhfCuot5l02oDl6+GIcHKjLJaVjotA37imLeelm/7bjpbZxOv+i5RaL+gMO21xUzLSufmtHRum5zPM7s6+9xMRzjkcfOAM3ajfdf0IraefZ4M+qjKd3Dz+CJ2nnUXHvF7eCrfyW3jbYy7I4e55d6zzmWx9qeNtzGp3H9xtQV9VBXn8eM70rl5vJ0f5xafU1u4vZ4FznRu7vccer51Q9BbzoLpjtg5NStW+7HepQFayguYlhXvgJiczzOezjM6IY41lTF3sp1x49O5a3IB672nK4gEmlg1L4fbxtu4ebyDaQuraestPcKhXaU8ONkR229ZOcxd09Rn3SJytqERQHuNJN3pJNtpJyO5i47mOpbkTWHBrsFeDSMEw2HAjKOggJkO62Wo0c/e1hDR832ks5650wt5wfNnjmElIyuLDHOUQ611LMmdzXr/1+6X/UYLtjbS3ncHWR3MKSlhfvZlDKB9BLzN7O/9W4S2Zt/5v69vFSMWWwajB8yfQbyVFez4RvXaXnn7PRVsbEpMKg/uKmFu+WHspfW85W2kZraJJncRP4/nsYivjEfWHCCjtJ4/eBt5yRWhqmApO+NZKuKv5oG8UtrNKSSf3XjYy/KCUvwpJWxrbmNPdQGmxhIW9N6YBdnrdvFIPVithousLcD2kgK2hp2sbGjif++u5T9SD7C+qJSW+CnvkKeAaSWNmFLM9NP6eddNZy0LFjYQzlnD694WXi9zEakv4qn6WO37ywtY0GhkZkVsv9TMNuF1F7A+fj8aaS/nkdIOUktq2dPcyEt5RprWvExbBKCd9fNK2GXM5aXXWvjfr60gPVDJAncj4Z597vYxqmgjrze38HqFi4inhMXf+htakYENrQCabOexTZW8tKmaX+/28laZEzMhmkrL2NlzJxr0UrUwdoc9bryDacW1xG6wg2zPzeDx5igQoj7PxriFjUQiAfauKWBS7x17Aat6LyTtrHLauDmtgFd726+P9cZOr+bQ2fUF63kwbSa1AaDjWX7U7/BqkFfXlOHrAmvORv7Q6uHXL1fz691NvF6WhTl6gB3VXnpWdyx+Rx/rjXAybWE1LT035WEPc+O1tDWV8oAznXHj7Uwr9nAoApGeoe15Hk7fqPt4KsvGzePj2xTf/mlOO+PG2/lxrptXewJwT/u55ezcnM9d452sagcinexckX96nznzeKq+/fQ6gl7WL3Rx13hbrJcjv5SdnbE221Y4+JH7z0QJsSM/PuzezxB8pLORVfNyTreR6+7TUxJg6/TY97K93cNT0x2xHovcUvZe4HxvTRkJh1vx9nwvER9N3i7M1pFnXfCCtNUW9/YijctyxXo0+twbhNtrWRDvLflxbil7A2ffOETY7+npFUnnNmc+z+wKDDDtI8yr89K5zT3A8L23mNucpbxa7+aB6U7uusPBA2u8HPJVsyDXxY+z7Px4Xi37IxDxurnrjqLTvxMAkSYW3+HgKW8ECLB3TT6T7jh9zJ/u6TlzCL59jZPbimvZvtDJuKwnWJOfzfKOLnYt7DOFJdDEqnmu2O/cHU4eKK7nIgcm+uyvnp47G+OyXCyoPX1cta9wcFtxLa+uyGfaZCd3ZeUwt9wXXx6re+7mRlbNczHJ6eAuZ36f3+XBrb/T44713o23M2lhLe29X9bpnr1xvd9lrPdsf7mLn1YfJlA3m3HOsss+FScYSmbC0jU8nW1jlMVKRt5CplgCtHcEgQgt9Y1Ectw84UjFYrGSMdvNHKuXrc2x7zTSZWZmpYeX8lPPHQHqaGRvxMmipTmMtZgYleFiZbGdzvr6+E1bF5F0N79tcJPdz03K+WsDa04pG8oKmJBqwWK1MbXAxejwYTpDxFt3ssFTR6Hd1M+Wn3/d4VCUUa5Sni90MNpiYbSjgPnZRtq9fiJEMKbns7J8DTPt8f2SN49sa4jOw+HYfqtrwJjfp/bZlezZ7SbTCARDkJrHyrLZZKZasKRm80SBnUiHDz8QSbYzv6KclVMyGGWxMNpewHyHkc4O3amJDGRoBdAzGBnlWsFjdgN0ednhDQN+1ucX8YI3TGZJORuWZhFpWsvcYg9BTNiL1jAjDSAZe8Eanp+dzqHaAn5W10okvYgXy/LJ6GqlttjN9q8ymmZyMN/txApgncyy8mKyzWd9JvJnmnxRMNzBInc2p8+jRka7Knnd284fK7IxAWGvm1kFL9MUSmFKyRIey7ZwrLmCR/LLYhdGoxGjAQjUsbwO7i8qZqI1gr+xlOWeAMZ0FxPMEPU1nx4q8jfTFgKDPYeJpghta/L5WV0Hhiw3G8oKsIVeY0lBKS3hPu13vsYLHpiQl0emJdaT8HjDAZKdbtaWuZmZEmBHaSHLm8LEejmK2NwcIHX2Gl4ssYOvgccX1nEISM1zk5duAAzY89fwfH76ufsx2Mji3BJqfRFseUtYNjsLk/81VucXsjV+PjcaATr4eWkjyXnFPGY3E+po4Kk1jZwv+5gy7Ng4wC5vLJhEOppp6Uomw27r+yWxv7yQuWW76TQ7ecxdzMzUEN66Rfy0NH5zEPGxvmgtTYfB5ipmvjPM5soze1KDu0qY5W6g0zKdZRWlzLQepr6kgPXt/UVQIzZXEY/lDNQrb4RgM7so4NevNfOH6hxCdSU8Um9mWYOHPzSWk3lgExu9EYx2F9kmLzua+wwfehtpMTqZYTcSrHezuNFEYYOXDw942TYbtheXsbefHWfASMT3Gu3ZlfyhcRVL66qZkZzMlE0d/LUiGyN+1s9bis9azLbWDt5vruT+yCYWuJvO+z30q7OSx1f4GO328NcD7fyxMotgeUlvDxVGA11NDbRnl/P67mb+2JBPpL6E1d6e/RmlraEJ27P17Gn28nqxiV3u0t6evwsKN7Pd5+DFxg7e272GjANreaoy1q0Y9JQwt7KLGdVe/vqujz2lqfjcRaz3w9iSep53GrDm1/J+s5uMi93uizR2dgUb8vocr5EAobCJUVYT0Em7P4ItI61PuEwlw2ai03cAAJPdxf22gSYfRQDjGcHUZDFjDBygMxxra2qeg4E6yM9fm5UJeS4ye3443Mne2kaOpWZhjx/2Ga48MvrLnlx43SZ7ARuezWFU778ECQYijEq1YsTI6Ow87u9tPMx+Tx1N4XQm2k2An7YOI6lGH0/lxm9o+05dsGTz9CY3E/vUFgyEwJqCFTDZcpiZ3TOlK0KwvZatPiOZOWkDbYzIt94QDqAAFtLTzECUUCAEvjp2HI5icBSxzOVggsvNfziS6WptoCloZLQjB4fVABhJdbqYardiyV7DtvrtbCsrYKqrhPmOZIh20NbxFYbBjVYmONIxAZjtTJniYPTZJ9NAgEAUsNj6GeY0YrH0nPqD7K1uJEAyU8qqeW72bOaXbWRZlgEOe9jq61Nfl5U5ZaXMdM3m+RIHBqK0+/xEjOlMcZgh6qMpvj37m70EMJDpcmIKN7PV8xGYc3hiaQ4TsvNZWXAHhBrZ7u0TH7rMzNxUx/NLC5hojdAZCAFmbNlO7nfl8cSmen7nqWeZ3QQkYy+uZVt9PS+VuJg6eyFTUoDDXtrDYLHlMNEKYGJUtoupGedeTg55qmnqAmteOTVLZzOnpIJfFKVBtIOt9e3Q21cZIaOgnKfzXMwvK8AOdB3wcd4+B7MDewr4G2Pzs/y7vIQMdrLtffo/I1421x8gashiWWUZ82fP5unqNUxJhtCuulhQ6/DQFALSF7Lh2dnMLKxgpavv3UaQprpmukhjhruIqY4cHnPnY+Mwuxo6+inMyNgpBcw577SQNGbkxHqtjLZ0Ug2Q4YpfcE1pZKRGCARCYLQz02mmzdMcn0MXoa2xFZNzOplGsLgq+WNjOfenmgATY3NysEUO0B7qr6wI0eQsZrtsWEz9hJb2BnaFsli01MEoI2CyMbMoB6P3tdhNzMVILeK3rR5WZscCgynDxcTUEJ0H+iTIVBdzHPFjxprDzPQwLU2n5wKasvK53xqr0zIllwmcPvYvKJrKzKIcRpvAaM2mMDeNY95WDhGkyePD5FrIHJsJI0YsjoXMyQjQ5LnSjx4GaSmtoMW2kEUOI9BFqAtMyWeeeJLNBiLhrgvfFKRlk0EzVfX+WO922M/22mZC0Vi7X6+2HvGRJfsUnjqQzobqEsZebNODcGzXs7zQ6WBRvu2Mf28ptnNzWiY/LQ8xZVMlM61AJEQgGKbF04G9dBd//XMjz9sDrC8q7ffGLNJZy/K6MDNKXH0CL4Q9BYxLy+BH+XUYizazob+uWhEB4DtXuoCv64wep84AIYDmEm4/o2MtQPthmNnPucDEYXZW1OH1BwhGIhCNAoav/3T8QIz9zF3q12HaO6NAOplpPSdvCzabFVoP09kZAnv8n81ppMZzi9FsjvWeRqKAkcycLMye12hp6gCHGa/3MBiymOEwQchPZxQINTArveGMtXceCEB2/C/JaWT0Ts80kpnjwNzcTH3BBHYkp2Cz25maV8BMG4AJY2g3VZVN+AMhwj27FIgOaqdG4sNWBmx9eiVH2dJI5gCBzsOE6QlpVlJt8QutyYzJAIQj5//uklOY6hhJbX0z3kA6h1o/wmAvYUJynyfwA4fxdwEpadh6jhljKhmpsKujE38nhAOxYy051dZ7AbLZbRjqPor/7TD7AwAH2OzKYHPfGjo7CZIymJ1xJqMZS0+uMBowkow5uefYMMZieSS29WPz8hg1/TWaAnnMNPvY5TOTXR3vm4t0squsgq3ew7FjnghdUSsZA+w4gyX1jItsX+HDAQJdrfwsffdZS9II9BdozyuMv76M9Z4/0xmKFRPpimLr81tuMFv71GLCZDESDn1EBBNgwJra95fcTLIJDoXCMGC/WR/JKVj75P9RVjOEOgkR4FAArK6+c5QtWK1GgoHQ5TtXXEikk53uIlYHnNRU5w34HcU+O8g2LTmsLPOx2D2DW8oNGEw2ZuQ7sXk7B3/qumBtNh6r28WMYCe+ugoW55fxUoObzAF7Pi9WhP21RfysGh6rrmTqWV99ptvDmwUB/I3VvLCwEFNdLfNTo0AU22w399tiN2YTSoqZ6ilhly/CxOzTATrsK2ducSOj3HU8d9ZUAVPOGl5PDxHqeI315YUsMNZT47oczxqIDH1DPIAGaO/4iNiFxwrxC16ycwU1BX2GPoxGzP2eA/z8fOFS6g+PZErZZhbZTfjL83i88Txn6+gFAs6FmK2kGsAf8OMPwpkdgEHavYcx2+2x3qR+9b/2gT5utOcwwfwaO1qbae8cyd4DYHC6mGCid39hnc6L5bl9LhJGDNZU4HD8r2cOyVmmVPJ6aiO7PE3s9f2Z9uYGOpobaSnzUGNvYnFxHR2mLJZtqibbGqBqXiH1hy+wX74SI6bT/znINxoYseU4sNZ5aKnfTWfAQGZRFiYuwSug+v1q0phdvYKpfZ72MJisfKVr7cW8siE1h5m2Sl5tCjAjtZGWZCe/sgGE2VtayPpAATWeajIsxthc36zqgdsynGfFBjCYc6lpLSWzv+UXMQUuWF/C3DojT1fvYqbNRGyubw47z/dDkXgRAy+8KOdu6aV/T8YlEW6nqmAR280L2VaXx+jeMpMxJ4O/Kwx9jrJQVxdGS/KgjrtR2aX8Z7abcDiC0WTC6HNzl9GMebAH7YC19TBisqYy1prKWJsJv7OArd6FZE65FAk0SMuKQp7ypbGyoSw+2nLW2i1WRlusjLalQaeD5XUdzC8zYzIaILlPsUYzZnOE9q7YtASAY7uKeaTsMI6yep529HNTY7QwOtXC6FQbG8IdTNpcz37X5enhFRnqhvAQfIRDnjKqOgBzbG6bJTUVMxDpAmtGBhkZGViNUSIRI6b+zm3BA7QfBsx2ZroyGG010hXo6mkeMMXv+kOE4mEt0tkxyGvqAEHVaGdKdjLwZ6rKGvu8piPCofqlLCiYzaT8Wo6RQkaqAThA24GeloJ0dISAZGypZ08uHYDRzhRHMgR8bK/dTQcGJuRkxS5EVhupBiDchdEW2182S2zTTcaB54iFO/10RtKZubSCX7/m5a+eeaTSRVtTBxF/B/4oGNJdzLSnMiq5K77vzn7OPDJAPjCSmp4KRPH7Tg+tHvIfoAtItaV9tfDWdw0ZOTjMUVpq6+k0pDMxy3RmzLCmYEsGAgfofStW2E97J2CI9YSarFaSga5Of/w7jODv8PfZyhTGpgKEiBhtZGRkkGEzxXaD0ZSAWGNliisdf2MjOzytWFyu+EWwk7aOCBl5ubHwCUQOdMR6wr8CkzUVS7iTzr7zLCNBgl/hISS/7wBGe348fAJhP22BMwuLBvx9fmfCBEMRLGZzfH9GCXT2eegoEiIUNjJqsMmpK3DGdhwLhMBsxYwVmxUC/r6/+UECgQgWqzXxETXiZ2vRInakruC3m84OeKlk2oz4fR19fr38tLVHsKUPYj5iJEh7k5f9ESMmU+w4bW/yEbbZyRjMhp6vtoCHBZPze+dxA735/tL0IodpX1PIUx1ZbDgnfPrZmp/DAk/fA7WnuAiQQoaNM845RAIEQkZGmWOfC3rdzCoLMbP63PDZtsbFJLf3jO0wGjk9/CMi5xhaAbTLx88XFrFgYREPTs9mmruZECOZ4o5PDrfnMiMFor5NLF5Tz6u1bh7Jnc0st4fO/s5wJjOjkoGQj621jWxf4WZ7eCQGovibPbQHrWTakoEDbF1TzquechZXdpz/gpOcHHu1if81NtZ6aDvnAQgTE0tKyTZDoLGESVkuHpxXENue0lZChjTmuPMYhYWJRS6sdLGrtIhVtfVUuRex3hfFkFbAbMdgL3tGMnOcJHOAHQ0dkOxgiiN+QTY6mTnFDF3NrF5YznZPNYvnzWZW/rOx+Y39CrHDnces/AIWb/awc5eH7fV/JgiMSk3BaLVgAaIdHrZ6PKwvrqbTlAwE2OvxcSwCxmQT0EVLbTlbmzrPufiMdhXE9k/9UhaU17J9czE/qzwAyXdQmGc7p6KLl85Uh5loNAq2nHOfqDU6KMxLwxBt5YWCUrbW17Kq6Fl2dYHVVRA71tJzmGgGOjaxeEUtW8tLWN7Yd5KchexcJ8mE2LHCTZWnnvULZ/PT/EI29vuIeIT9u6rZ6r10r22x5LjI9Fey3mtlxpSe4WMLo8zg9/kIEn/bQO0BjIYuQqHBxQCDMUKwM0A4EoGM6UyxdlBVFnvzApFOdpbmMamo/qLfgWi2mon4W2NP0If9vFrWQMAEXX1TYaCZzfE3CYTbq6ltN5E55fQxEW6tY6s/9lTz/vo6WrCTbR/k74rhANsrvQQjQNDL5oZORjmyGI2FCbl2wp5NbI+3faypgq3tqcx0xdZtBMKBjwheaArIJXCodinrwzmsLEqHYJBgz59wrJLMfBfGXRWsb+okGA7QtrmM7V1O5uTEDvRIOPb5Y6H49ItQ7O/hCGAMsLOsiMfXNHEoHOZQUxnL66NMLciJT2KIEA4GCQYDdEUgEg7F1x25cG3WdDKMHfy8tJqWziDBYCd711SwN5LOlPhQdjj++dix2BX/+XB8n55/3RFfBYs9ycwvyyc10nfdESCVjLQILeXPst0XIBgMsH9XKRu9JiY40wELU/KdRDxrWeUNEA7HazPlMMNuhHATq92t2EpKmWIOn247XpvNnkJ4VxnLPe0cCwY51l7P6mo/lqzJjL7Mx4PIUDXEhuA/oqM5PsfOYMaWlcvMooXM7B3HtvHEyxuJrKhgV/2zLCGZVHs+v3i2pP+7d6ODRWW5HCr10FReRmBKMWvrzGzPL6K+aRPbXS6ed68hL/AsO1rreCHk5LHSIshfSlMk3P+FxuJkjqsBv+cAO6sbSLX3eeqzhzWHlzxmtlZUs8PbQXvrAUg2M9qZT2FJMVNTY8Wa7KVsq0xmdeVrbC9rheSR2HKW8HTp7Isa0jHac5iY/Bo7uiDZPj02/B5bwoTSWl40lPJCYx3LW8Gc5uA/Xl7BnFQG6JawMqeykmBpBTuqS2nqimIwp5GZv5GVRTYwmllZ4Gd5bSs/XxNgQtEathU1s6DgZbzVdbS57EzNm4e9uQKfr4GtlnRmuM7ehzlsqINVpZU01a6liWRSM6az1u3m/ksyncpIRs4dJHt2k5rT31O1RsaWbKYmuYz1DY2sL41gNKeSXbCGZSX22A2I0cGyimKC7mraPJsIZeSyrMTAU+7dROKdHpYpa6jpKmNVdTMvuHeTbE1nRukKlk2xwDmv2I7g91Tyc0v6BR5EuggmJzMcZbSFppPd26SVGe4iWkqW8uPxS7HYnPxHWTkTTPksXugiua7yArMl05nqSmVBWTY/9q5hz8sunni5nMiKCn56RykRTIxyTGdD+QXmJPZjbL6bmb6lPJhVj9GSzozSMl60lzJrRSEPWOt5GjA4cnH43Ewq7SCImcyicpb1BkwDGTk5BMvzuM0XIGJJZ2bZCqYOogM0QhQsOcy2eXjEWcShsJFRDjcvFsUCpmVKOS+FlrK6wMGqMJisdqZUVDLfBmAk05WDpbiEHzkn84vWCiZetm7RAN7mA0QPHGCWs+6MJebcWv74rB1jRjG/KivjqbI8fhSIYE5z8kR1afwJ7gh73dnx19HFLHdOYDkGHOU+aqZk8ESlm5D7WabZQxit6Ux8tpaVPTet/kp+6nr59ChQRyE/agBS5vG73Xn4LlDb/OpKwqUVPDW9glDUgDnNwfxNK7jfAuBjdc5sdvTex1Xwb44KIJ1lzfXMCZ9v3SVEm7wEuj5itWsCq/uuPK2YN18rIKOkmg2U8UJxDstDkGy1MXHpZpZlx7bNlF3Kr0KlLHHncHsIrOk5rKyOv4bJ10hLKESXe8pZk3XuYKW3jpnZZfzq2VJWVxYyyd0FySPJyCnjV0szvqmTOESuuH85FvjoH5eyQXPZav5v2bpL2aSIfCWdbJ2eR8vsXdS4hv7TuO1rnDwYcPPXTdn9XNQ7qZruoiWviV/nDf1tFRH5Jvrv7icJuZddkraGWA+oiAxKJEh7bSk/78qhJkeBTEREvlkUQEX+2YQbmZtVQpvVydOV7sE9PHK5+cuZll8XmyfaD6PFxUvNAzxJP6TW7eWprBJ2hgdY2XeGczVf8sV/9b846X9czT/+zxcMsBhjdjl/rOivB1hEZGjRELyIiIiIXNClHIIfWk/Bi4iIiMiQpwAqIiIiIgmlACoiIiIiCaUAKiIiIiIJpQAqIiIiIgmlACoiIiIiCaUAKiIiIiIJpQAqIiIiIgmlACoiIiIiCaUAKiIiIiIJpQAqIiIiIgmlACoiIiIiCaUAKiIiIiIJpQAqIiIiIgn1ncvR6LsHj1yOZkVERETkCrFfwrYuSwB1ZNguR7MiIiIicoVELmFbGoIXERERkYRSABURERGRhFIAFREREZGEUgAVERERkYRSABURERGRhFIAFREREZGEUgAVERERkYRSABURERGRhFIAFREREZGEUgAVERERkYRSABURERGRhFIAFREREZGEUgAVERERkYRSABURERGRhFIAFREREZGEUgAVERERkYRSABURERGRhFIAFREREZGEUgAVERERkYT6zpUuYDC87f4rXYKIiIjIkOLIsF3pEgY0JAIofLN3ooiIiMjl1BX+YtCf/eLECQ4fP3kZq/n6NAQvIiIi8k/iixMn2PfWH650GRekACoiIiLyT2IohE9QABURERGRBFMAFREREZGEUgAVERERkYRSABURERGRhFIAFREREZGEUgAVERERkYRSABURERGRhFIAvQzer1nMxLV/IXIpGutu4X/mPkHVsUvR2GU0VOoUERGRK27I/K84h5IxUx9nQ9SMcaAPHPsLu0+MYfItIxJZ1jdb9AjNraew3zOG5ASs7rN36ql4xYvv71/AiBvIuHsWxQ+lc0OsGI7s28Lahrc5GDzF8O+N59/nF/LTHww73cDnfn5dUcnmD8aw/JXHcRpOL4ocbqGiZgetH4SIJt1AxqRZLHkone+e3liO7NvCyiov0amreeWhmy6itiBtr2xh81sHOXriFAbrGO5xPUzRPSNPH2/nqQ2g62/NrK3YRus1s9ix1tmnrq+/J5CNMQAAD/NJREFU7o/bXqFim5f2T08x/Dob984tZP6t8eM8eoTdVTVsbTvKJ6eSuD7ldv59/iz+LSW2X7sON1NZ9Sa+wx9znKsZc+u9FM2/j1uvudhvV0REvunUA3oZGK+7iXGjhg24/MO3GtjqCyWwoiHggzep/P1BvkzEuo69ydNr9/FlViEv/6qSlxdNILpnA2v3BAGIvLeNJ2uOMPbRUup/tY5Vd5/iN89tpvnz2I9HDr/BgiU17B8xkuFnt93dQcVzWzhozWXDL6uoe+Y+hrdu4uk3gvEPnMC7cRlP/h6uvy7pomv78JV1PN2axLTiWG3rpl6Fr3Idm/82iNqAI/vWMa/iTwy3Xo3h7IVfd91/q+fJqoPcOGsZdb9cx6qfJNFa8wbvRAGitFWto+KDkcx5Zh07Ni5jjuUglc81xJZ3/4W1zzRw9Pu5rNtYyY7yQjJO7ODJjS18NtjvVUREhoxvTw9odwtPzG1mzKIfcnRnCwc//QK+N4Plc83sq9mBLxDiS8N4Cp8pYvKwP/LE/G1cu+RF3Lf0XKZPsPuZx9maUkrD3Bv4cN8WKhre5mDwJIy4EbtrLkvui/XevV+zmMWfzmLnkh9ysGYxT35+L4Wn3qTyg/H8z7sP8r88HxNlNRPb7mFD1UOM+/QvbPzlDlo/6On5uY+iR52MGzjDDiB6/rp+uZAnu++jaNjb/Of7QY53JzHm7rksf8hGMh/x6+JltGcVcuN7b+ALfMGXjOSeuYUsyrQMvoIP3mTZ82/gC5xk+PfupLD4YSaPiu3Dz96pZ22Nl/ZPT8Iw8+netfdqcD3n5fgpL/m5LcwpX8ODoy522wev68Qprr/7YQpnxHv2rrmPf898g6ffOUJk0tX49vyJaNbjFN4a69n77n2zeOCtZfyn7wTOSSOIdl/NtCWrmRzdhsv3xZmNf/AnWqO3s2TuXdxsAK6ZQPFDb5PX0MyH9+VxMyeJfn8Wv1g0Bt8zf+I3F1mb4fv3Umy/k8nx3tjvTrqPrB2rORrohh8MO39twJfczvIKJ8P3LGWf71KuOwnfzn0YppayKHMkADfc9ziv3NfTeojj2Jgz/2Em/yB2PEx+yMnri1rY/yncOmwYWQ8tJGNST2/rCOb8xMbrDUf4BM7opRURkaHv29MDmpQEHGVfm5nitetp2FjI2L838GTFu9gXreGVqnUUWt5l8w4/DBvPtFtP0brHf3oe5+fv8uYHZu65+yY4toOVVX5unLuanTtq2PHUeI5vq+ztCerLQBLR91vYb3+c+o2zmPpQKUvsSVw/dRl7qx5iHEeoem4z7dflsqFmC3urHufeUzt4euNf6LrYbbxQXYYkvvQ1sz9zIb/auAHP2nuJ7qmkMtZFBZyifc/bjHm0lFeqNvHyQ1exb+OW3p6/Cwvx5u+DTHvmRfa+sprC696lonIfH0O8d+1PDHctY0fDFnaunMHwtzaw8o0g3DKXX8wdjeF7s6hruLzhEyD5lvtY8ehd8aADcILjn57ieqsFIx+z/++nGPP9G/tMoRjJ2JSrOPrekfjPT2Byyjn9h3GngKQzeheTR1yN4dOjHO2OteWclD5goDp/bQZuynT2BkDo5sN9b9LaPYasW4YNojYYd8/ANzZfb91Haf8giRuT/JQtWcjE3IdxFb/Ib//WHf/8SCYvKuLBW/rU9nmQ40kjuP4a4Bobk3vDJ0Q+9fOb3x/k2lt/yJgBt0ZE5Ntpe30DpqtHnPNne33DlS5t0L49ARSAJDIm3RW7+A+7ibHXwfBbJpB5DcAIxqRczZefBuliGPZ7bsfwjhdf/Pr52Tt/Yr91AvemAKNm8Iua1RRnxi7MyT+YQJb1C47+/UQ/qzxFdNh4HrjnJr47rJ9g8Ldm9n0+njlz07nBEKvr33LvPGPdgzaYuqwTeKBnTt51dzLt+yfxtR3tXTz81nuZfF2szu9m3YMdP60fRBmUU5AxdQaZ1xlg2Egmu27n2sNv0/45HPG1cNB6L4X3jCQZMI66izl3mzn4/74dC6hX0MetW9gcGM+cqTcBJzneDcOHnZnSho9IItp98sI3Bd+7nbG8zW/2HIndvHQf4Xc73+b4qW6OX+z3eU5tp7WVF/L/zJjPz7Z9wT1LFvNv111825d03dETfHLiJL63DpIxfx07X1nHkluCbH5+C97+trv7IFVVXoZPyiWr764+9gaP5M7iJ4++SKvlYTY8aht4LrWIyLfUzLxcqqqqzvi3qqoqZublXqGKLt63ZwgegKu5tvdil4QhCYaPuLp3qWFYUqwDCzCOm0DWsBd5851uHFngaz3ImLsfJnYpPsnBPdvY/Jafo5/HfiB68hRjen74LIYRI7l+gIq6AiE+OfkuT+f96awlN/LJ58BFPYBx4boMI8x9ahnG8GuS+PJEkAg3AUlcb726T3sjGD4M9p84CefOGDxX0tVcb+2TJq4ZybX4OXoCrj0cAqu5T+8aXJ9igbc+4hMYcP9cXlE+fGMDT3tgzjOLcZ5vX/f/1Z7rmrsoXuRn5cZl/GRbEoZhN3Lv1NsZ887HGPqZ8vlVa8uYu5q6GSEOtr7B5rXrGP7cMh48T8/nxfkK67aeAk4xZuosJqcMA4aROSuXe/ZtYt97URyZfXs+Oyh/bjP7v7+QdQ/ddGbAHHUPq8pv53jQz+uvbGFxeRK/KPmhhuBFRM7SEzbnz58/5MInfOsCaD8GCgUGG9Myr2LxvnfpuhX2fXAT9z4amwv52Z5NPLkziaJn1sWf4A3y2+In2TfgOs6TPAxgGHEP62rmcmt/yy+i1+yi6wKIwsA7YbCpq0fSYGLqN8QJ2n65jrXv30Tx2vk4ensQr+LaYXCwuxs4HaaPd5/EcM1Vg3pC/4bMubyUOYuu7lMYhg3D+F4VrqS+Nz9ftbbTjNdYuOkaCzel3AiBIip2HuLBRbbBruAyrHsEw5OSYFifY8kwgmuvOcX+7lP03MBEjjXz9PIdRKc+zoYZ/b3xYBg3jBrGDaNGMm5EkIeWvEHrpz+8LD28IiJDXU/oHGrhE751Q/AX5+afOLn2Ay9v7vOyP8VBVvwiePC9IxjGTep9fQzdR2j/9GLDWkyy5Qau7f6Yo33nWUZP8NlXGK4dTF3RYOyhjvgHOH7iFNeOGBHvhTrFJ4E+T+dHT3C8O4nrR1w1yApCHP20z3D95x/xCSO48Tq4PsUMgY850ufTRw8H4bqBe4cvn27er1nH2g/Gs/yckHUDGd9L4uD7B/u8x/UI7R+cYsz3bzqnpXNET/B+WwcfRg0kDxuGEXjf5+fLFBtjB5XOz1fbEX77zBMs29d3qkdP4BvkNInLtu4bGJsCB98/PZ2DaJBPPk/i+mvin/u0hZXL38Awq5R1Z4XPrrYqHiqu5/1+qroUWyYi8s9qKIZPUAA9v1F3Mm2Un83bDjL27tt7hwGvve5qon9/l/e7ge4j7K7ZxyfD4MvAuU8d98cAfPlpkM+6o0R+MIF7rjvIb2paOBIFoh/RXFVK/trmi54bOai6Pn2b37QGiQBdf3uD//zgKjKybuxd/OU7b/Lbw91AlA/37MGHjaxbBt+vuX/njtj6o0F2e97my5Q7yRgGN9knMCbQzNbWj4gAkWMtbH3rC8befTs3AAYM0H2Eo93RS/MC//OIvNfAyn3D+PdF93Jj9ASffR7/0x0FDGRMdWBobWBz20d81h3knR3beL37dh7Iis2djXTHPv/xidiw85cnYn/vigKGEPtqNrCy5i8c6e7mSNsrVOw5xT0z7owfP1G6Pj/BZ5+H+PIURLv7rvtCtd3A2JRT+LZt4XfvBfns8yAftm5h6ztXYbePuXBtEF/3CY6fOAWc5PjnJ/js8+7Yd/K11j2Ce6beTnTfK2x8J0hX90d4axpoHXYn944zAEF+t3EbB2+dRdGtSXz5+en2u6KQ/H0b13+6h4qav/Dh5yf47JifX7/i5RPrD7Gr91NE5J+OhuDPy8I9d49h87GruDfz9PjpzVNnMe39zSyeuw/DiDHc+2ghy8dtYXHVOhZYSik6b5sGMu6+k2srNjFj/p2sqili/jMLif6ygZ89tIUoV3H9rQ6WFzvPmC85GIOpy3CrE/t7m8n/5UGOczUZuUUU9QbMJMZm3cnxV0qZ8n6I6IgxTFv0MM7BDB1HTwE3cO+kEfxmyUJ8n55kuPVOlvRsx6h7WbXkBGtrVjN140kYdgP2qY+zfFJsWsN3b3Vg37GFJ+f6eWDlBhb94CI3/iIc9L3LJydDVJYUUdl3wfdyqau4j5t+kMu6RdtYW1PKjOAprv3e7RQ+8zCOYQBRWjc+zkrf6Z7likeLqCAJe3EV67PGULhkFsc3bmHeQ19gsIwma/4yim+N78TDO/hZyU56+wk/WM+M3wPWqby8MY/oBWobN+tJlrONzRVPUnEChltuJGvukxRlDhtEbYeoXLSaN0/2LG1g3iMNwGiKfvksY7/WuiE582HWndjC2o1PMvUEXP/9Oyl+Zha3GoBuP63vn+ST9zeQ91bfxpPIemoLqzMnsPyZk1Rs2cbiR0N8ydXcOO5Oli+5j0H0O4uIyBDzL8cCH/3jUjZoLluNcWPlhT94EbztfhwZl2J+28V7v+YJnu6eRf2i9CH/NG7f95Oeuy2x94D6Jr3I/5qk/0OTiIjIN0lXeHCjrB7PawCkpNsveXaKLCoi5F52SdrSEPyAonz8zitU7EviAdfQD58iIiIi3xQagu9XkN8ueZLKYzdw76OP88BlfjH6oByu55Fn9nB0gGedDCMcrKoa4En6r62Dsrmb2Nc9wMqTbmDOc2t4MOVSrKub5rWPs/adgR7q+ldM3/lXwv/1X/0v/m//g2T+D13/X/+LL+9+EhERkcHQELyIiIjIN5yG4EVEREREvgYFUBERERFJKAVQEREREUkoBVARERERSSgFUBERERFJKAVQEREREUkovQdUREREZAg4evQIb7/9zpUu45JQD6iIiIjIN9w/U/iEIdQD6m33X+kSRERERK6QJFLS7Ve6iEtmSARQ/V+QRERERP55aAheRERERBJKAVREREREEkoBVEREREQSSgFURERERBJKAVREREREEkoBVEREREQSSgFURERERBJKAVREREREEkoBVEREREQSSgFURERERBJKAVREREREEkoBVEREREQSSgFURERERBJKAVREREREEkoBVEREREQSSgFURERERBJKAVREREREEkoBVEREREQSSgFURERERBLqO5ej0ciiosvRrIiIiIj8E/iXY4GP/nGlixARERGRbw8NwYuIiIhIQimAioiIiEhCKYCKiIiISEIpgIqIiIhIQimAioiIiEhCKYCKiIiISEIpgIqIiIhIQimAioiIiEhCKYCKiIiISEIpgIqIiIhIQimAioiIiEhCKYCKiIiISEIpgIqIiIhIQimAioiIiEhCKYCKiIiISEIpgIqIiIhIQimAioiIiEhC/f/WYNby50m5vwAAAABJRU5ErkJggg==","type":"image/png"}],"permission":0,"title":"sdfsdf"}},"meta":{}}`
