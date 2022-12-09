package service

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/1Panel-dev/1Panel/backend/buserr"
	"io/ioutil"
	"math"
	"net/http"
	"os"
	"path"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/1Panel-dev/1Panel/backend/app/dto"
	"github.com/1Panel-dev/1Panel/backend/app/model"
	"github.com/1Panel-dev/1Panel/backend/constant"
	"github.com/1Panel-dev/1Panel/backend/global"
	"github.com/1Panel-dev/1Panel/backend/utils/cmd"
	"github.com/1Panel-dev/1Panel/backend/utils/common"
	"github.com/1Panel-dev/1Panel/backend/utils/compose"
	"github.com/1Panel-dev/1Panel/backend/utils/files"
	"github.com/joho/godotenv"
	"github.com/pkg/errors"
	"gopkg.in/yaml.v3"
)

type DatabaseOp string

var (
	Add    DatabaseOp = "add"
	Delete DatabaseOp = "delete"
)

func execDockerCommand(database model.DatabaseMysql, dbInstall model.AppInstall, op DatabaseOp) error {
	var auth dto.AuthParam
	var dbConfig dto.AppDatabase
	dbConfig.Password = database.Password
	dbConfig.DbUser = database.Username
	dbConfig.DbName = database.Name
	_ = json.Unmarshal([]byte(dbInstall.Param), &auth)
	execConfig := dto.ContainerExec{
		ContainerName: dbInstall.ContainerName,
		Auth:          auth,
		DbParam:       dbConfig,
	}
	out, err := cmd.Exec(getSqlStr(dbInstall.Version, op, execConfig))
	if err != nil {
		return errors.New(out)
	}
	return nil
}

func getSqlStr(version string, operate DatabaseOp, exec dto.ContainerExec) string {
	var str string
	param := exec.DbParam

	if strings.Contains(version, "5.7") {
		if operate == Add {
			str = fmt.Sprintf("docker exec -i  %s  mysql -uroot -p%s  -e \"CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED BY '%s';\" -e \"create database %s;\" -e \"GRANT ALL ON %s.* TO '%s'@'%%' IDENTIFIED BY '%s';\" -e \"FLUSH PRIVILEGES;\"",
				exec.ContainerName, exec.Auth.RootPassword, param.DbUser, param.Password, param.DbName, param.DbName, param.DbUser, param.Password)
		}
		if operate == Delete {
			str = fmt.Sprintf("docker exec -i  %s  mysql -uroot -p%s   -e \"drop database %s;\"  -e \"drop user %s;\" ",
				exec.ContainerName, exec.Auth.RootPassword, param.DbName, param.DbUser)
		}
	}

	if strings.Contains(version, "8.0") {
		if operate == Add {
			str = fmt.Sprintf("docker exec -i  %s  mysql -uroot -p%s  -e \"CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED BY '%s';\" -e \"create database %s;\" -e \"GRANT ALL ON %s.* TO '%s'@'%%';\" -e \"FLUSH PRIVILEGES;\"",
				exec.ContainerName, exec.Auth.RootPassword, param.DbUser, param.Password, param.DbName, param.DbName, param.DbUser)
		}
		if operate == Delete {
			str = fmt.Sprintf("docker exec -i  %s  mysql -uroot -p%s   -e \"drop database %s;\"  -e \"drop user %s;\" ",
				exec.ContainerName, exec.Auth.RootPassword, param.DbName, param.DbUser)
		}
	}

	return str
}

func checkPort(key string, params map[string]interface{}) (int, error) {

	port, ok := params[key]
	if ok {
		portN := int(math.Ceil(port.(float64)))
		if common.ScanPort(portN) {
			return portN, buserr.WithMessage(constant.ErrPortInUsed, portN, nil)
		} else {
			return portN, nil
		}
	}
	return 0, nil
}

func createLink(ctx context.Context, app model.App, appInstall *model.AppInstall, params map[string]interface{}) error {
	var dbConfig dto.AppDatabase
	if app.Type == "runtime" {
		var authParam dto.AuthParam
		paramByte, err := json.Marshal(params)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(paramByte, &authParam); err != nil {
			return err
		}
		authByte, err := json.Marshal(authParam)
		if err != nil {
			return err
		}
		appInstall.Param = string(authByte)
	}
	if app.Type == "website" {
		paramByte, err := json.Marshal(params)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(paramByte, &dbConfig); err != nil {
			return err
		}
	}

	if !reflect.DeepEqual(dbConfig, dto.AppDatabase{}) {
		dbInstall, err := appInstallRepo.GetFirst(appInstallRepo.WithServiceName(dbConfig.ServiceName))
		if err != nil {
			return err
		}
		var database model.DatabaseMysql
		database.Name = dbConfig.DbName
		database.Username = dbConfig.DbUser
		database.Password = dbConfig.Password
		database.MysqlName = dbInstall.Name
		database.Format = "utf8mb4"
		database.Permission = "127.0.0.1"
		if err := mysqlRepo.Create(ctx, &database); err != nil {
			return err
		}
		var installResource model.AppInstallResource
		installResource.ResourceId = database.ID
		installResource.AppInstallId = appInstall.ID
		installResource.LinkId = dbInstall.ID
		installResource.Key = dbInstall.App.Key
		if err := appInstallResourceRepo.Create(ctx, &installResource); err != nil {
			return err
		}
		if err := execDockerCommand(database, dbInstall, Add); err != nil {
			return err
		}
	}

	return nil
}

func deleteAppInstall(ctx context.Context, install model.AppInstall) error {
	op := files.NewFileOp()
	appDir := install.GetPath()
	dir, _ := os.Stat(appDir)
	if dir != nil {
		out, err := compose.Down(install.GetComposePath())
		if err != nil {
			return handleErr(install, err, out)
		}
		if err := op.DeleteDir(appDir); err != nil {
			return err
		}
	}

	if err := appInstallRepo.Delete(ctx, install); err != nil {
		return err
	}
	if err := deleteLink(ctx, &install); err != nil {
		return err
	}
	backups, _ := appInstallBackupRepo.GetBy(appInstallBackupRepo.WithAppInstallID(install.ID))
	for _, backup := range backups {
		_ = op.DeleteDir(backup.Path)
	}
	if err := appInstallBackupRepo.Delete(ctx, appInstallBackupRepo.WithAppInstallID(install.ID)); err != nil {
		return err
	}
	return nil
}

func deleteLink(ctx context.Context, install *model.AppInstall) error {
	resources, _ := appInstallResourceRepo.GetBy(appInstallResourceRepo.WithAppInstallId(install.ID))
	if len(resources) == 0 {
		return nil
	}
	for _, re := range resources {
		if re.Key == "mysql" {
			database, _ := mysqlRepo.Get(commonRepo.WithByID(re.ResourceId))
			if reflect.DeepEqual(database, model.DatabaseMysql{}) {
				continue
			}
			appInstall, err := appInstallRepo.GetFirst(commonRepo.WithByName(database.MysqlName))
			if err != nil {
				return err
			}
			if err := execDockerCommand(database, appInstall, Delete); err != nil {
				return err
			}
			if err := mysqlRepo.Delete(ctx, commonRepo.WithByID(database.ID)); err != nil {
				return err
			}
		}
	}
	return appInstallResourceRepo.DeleteBy(ctx, appInstallResourceRepo.WithAppInstallId(install.ID))
}

func updateInstall(installId uint, detailId uint) error {
	install, err := appInstallRepo.GetFirst(commonRepo.WithByID(installId))
	if err != nil {
		return err
	}
	detail, err := appDetailRepo.GetFirst(commonRepo.WithByID(detailId))
	if err != nil {
		return err
	}
	if install.Version == detail.Version {
		return errors.New("two version is same")
	}
	tx, ctx := getTxAndContext()
	if err := backupInstall(ctx, install); err != nil {
		return err
	}
	tx.Commit()
	if _, err = compose.Down(install.GetComposePath()); err != nil {
		return err
	}
	install.DockerCompose = detail.DockerCompose
	install.Version = detail.Version

	fileOp := files.NewFileOp()
	if err := fileOp.WriteFile(install.GetComposePath(), strings.NewReader(install.DockerCompose), 0775); err != nil {
		return err
	}
	if _, err = compose.Up(install.GetComposePath()); err != nil {
		return err
	}
	return appInstallRepo.Save(&install)
}

func backupInstall(ctx context.Context, install model.AppInstall) error {
	var backup model.AppInstallBackup
	appPath := install.GetPath()

	backupAccount, err := backupRepo.Get(commonRepo.WithByType("LOCAL"))
	if err != nil {
		return err
	}
	varMap := make(map[string]interface{})
	if err := json.Unmarshal([]byte(backupAccount.Vars), &varMap); err != nil {
		return err
	}
	dir, ok := varMap["dir"]
	if !ok {
		return errors.New("load local backup dir failed")
	}
	baseDir, ok := dir.(string)
	if !ok {
		return errors.New("load local backup dir failed")
	}
	backupDir := path.Join(baseDir, "apps", install.App.Key, install.Name)
	fileOp := files.NewFileOp()
	if !fileOp.Stat(backupDir) {
		_ = fileOp.CreateDir(backupDir, 0775)
	}
	now := time.Now()
	day := now.Format("20060102150405")
	fileName := fmt.Sprintf("%s_%s%s", install.Name, day, ".tar.gz")
	if err := fileOp.Compress([]string{appPath}, backupDir, fileName, files.TarGz); err != nil {
		return err
	}
	backup.Name = fileName
	backup.Path = backupDir
	backup.AppInstallId = install.ID
	backup.AppDetailId = install.AppDetailId
	backup.Param = install.Param

	return appInstallBackupRepo.Create(ctx, backup)
}

func restoreInstall(install model.AppInstall, backupId uint) error {
	backup, err := appInstallBackupRepo.GetFirst(commonRepo.WithByID(backupId))
	if err != nil {
		return err
	}
	if _, err := compose.Down(install.GetComposePath()); err != nil {
		return err
	}
	installKeyDir := path.Join(constant.AppInstallDir, install.App.Key)
	installDir := path.Join(installKeyDir, install.Name)
	backupFile := path.Join(backup.Path, backup.Name)
	fileOp := files.NewFileOp()
	if !fileOp.Stat(backupFile) {
		return errors.New(fmt.Sprintf("%s file is not exist", backup.Name))
	}

	backupDir, err := fileOp.Backup(installDir)
	if err != nil {
		return err
	}
	if err := fileOp.Decompress(backupFile, installKeyDir, files.TarGz); err != nil {
		return err
	}
	composeContent, err := os.ReadFile(install.GetComposePath())
	if err != nil {
		return err
	}
	install.DockerCompose = string(composeContent)
	envContent, err := os.ReadFile(path.Join(installDir, ".env"))
	if err != nil {
		return err
	}
	install.Env = string(envContent)
	envMaps, err := godotenv.Unmarshal(string(envContent))
	if err != nil {
		return err
	}
	install.HttpPort = 0
	httpPort, ok := envMaps["PANEL_APP_PORT_HTTP"]
	if ok {
		httpPortN, _ := strconv.Atoi(httpPort)
		install.HttpPort = httpPortN
	}
	install.HttpsPort = 0
	httpsPort, ok := envMaps["PANEL_APP_PORT_HTTPS"]
	if ok {
		httpsPortN, _ := strconv.Atoi(httpsPort)
		install.HttpsPort = httpsPortN
	}

	composeMap := make(map[string]interface{})
	if err := yaml.Unmarshal(composeContent, &composeMap); err != nil {
		return err
	}
	servicesMap := composeMap["services"].(map[string]interface{})
	for k, v := range servicesMap {
		install.ServiceName = k
		value := v.(map[string]interface{})
		install.ContainerName = value["container_name"].(string)
	}

	install.Param = backup.Param
	_ = fileOp.DeleteDir(backupDir)
	if out, err := compose.Up(install.GetComposePath()); err != nil {
		return handleErr(install, err, out)
	}
	install.AppDetailId = backup.AppDetailId
	install.Version = backup.AppDetail.Version
	install.Status = constant.Running
	return appInstallRepo.Save(&install)
}

func getContainerNames(install model.AppInstall) ([]string, error) {
	composeMap := install.DockerCompose
	envMap := make(map[string]string)
	_ = json.Unmarshal([]byte(install.Env), &envMap)
	project, err := compose.GetComposeProject([]byte(composeMap), envMap)
	if err != nil {
		return nil, err
	}
	var containerNames []string
	for _, service := range project.AllServices() {
		containerNames = append(containerNames, service.ContainerName)
	}
	return containerNames, nil
}

func checkLimit(app model.App) error {
	if app.Limit > 0 {
		installs, err := appInstallRepo.GetBy(appInstallRepo.WithAppId(app.ID))
		if err != nil {
			return err
		}
		if len(installs) >= app.Limit {
			return buserr.New(constant.ErrAppLimit)
		}
	}
	return nil
}

func checkRequiredAndLimit(app model.App) error {

	if err := checkLimit(app); err != nil {
		return err
	}

	if app.Required != "" {
		var requiredArray []string
		if err := json.Unmarshal([]byte(app.Required), &requiredArray); err != nil {
			return err
		}
		for _, key := range requiredArray {
			if key == "" {
				continue
			}
			requireApp, err := appRepo.GetFirst(appRepo.WithKey(key))
			if err != nil {
				return err
			}
			details, err := appDetailRepo.GetBy(appDetailRepo.WithAppId(requireApp.ID))
			if err != nil {
				return err
			}
			var detailIds []uint
			for _, d := range details {
				detailIds = append(detailIds, d.ID)
			}

			_, err = appInstallRepo.GetFirst(appInstallRepo.WithDetailIdsIn(detailIds))
			if err != nil {
				return buserr.WithMessage(constant.ErrAppRequired, requireApp.Name, nil)
			}
		}
	}

	return nil
}

func handleMap(params map[string]interface{}, envParams map[string]string) {
	for k, v := range params {
		switch t := v.(type) {
		case string:
			envParams[k] = t
		case float64:
			envParams[k] = strconv.FormatFloat(t, 'f', -1, 32)
		default:
			envParams[k] = t.(string)
		}
	}
}

func copyAppData(key, version, installName string, params map[string]interface{}) (err error) {
	fileOp := files.NewFileOp()
	resourceDir := path.Join(constant.AppResourceDir, key, "versions", version)
	installAppDir := path.Join(constant.AppInstallDir, key)

	if !fileOp.Stat(installAppDir) {
		if err = fileOp.CreateDir(installAppDir, 0755); err != nil {
			return
		}
	}
	appDir := path.Join(installAppDir, installName)
	if fileOp.Stat(appDir) {
		if err = fileOp.DeleteDir(appDir); err != nil {
			return
		}
	}
	if err = fileOp.Copy(resourceDir, installAppDir); err != nil {
		return
	}
	versionDir := path.Join(installAppDir, version)
	if err = fileOp.Rename(versionDir, appDir); err != nil {
		return
	}
	envPath := path.Join(appDir, ".env")

	envParams := make(map[string]string, len(params))
	handleMap(params, envParams)
	if err = godotenv.Write(envParams, envPath); err != nil {
		return
	}
	return
}

func upApp(composeFilePath string, appInstall model.AppInstall) {
	out, err := compose.Up(composeFilePath)
	if err != nil {
		if out != "" {
			appInstall.Message = out
		} else {
			appInstall.Message = err.Error()
		}
		appInstall.Status = constant.Error
		_ = appInstallRepo.Save(&appInstall)
	} else {
		appInstall.Status = constant.Running
		_ = appInstallRepo.Save(&appInstall)
	}
}

func getAppDetails(details []model.AppDetail, versions []string) map[string]model.AppDetail {
	appDetails := make(map[string]model.AppDetail, len(details))
	for _, old := range details {
		old.Status = constant.AppTakeDown
		appDetails[old.Version] = old
	}

	for _, v := range versions {
		detail, ok := appDetails[v]
		if ok {
			detail.Status = constant.AppNormal
			appDetails[v] = detail
		} else {
			appDetails[v] = model.AppDetail{
				Version: v,
				Status:  constant.AppNormal,
			}
		}
	}
	return appDetails
}

func getApps(oldApps []model.App, items []dto.AppDefine) map[string]model.App {
	apps := make(map[string]model.App, len(oldApps))
	for _, old := range oldApps {
		old.Status = constant.AppTakeDown
		apps[old.Key] = old
	}
	for _, item := range items {
		app, ok := apps[item.Key]
		if !ok {
			app = model.App{}
		}
		app.Name = item.Name
		app.Limit = item.Limit
		app.Key = item.Key
		app.ShortDesc = item.ShortDesc
		app.Author = item.Author
		app.Source = item.Source
		app.Type = item.Type
		app.CrossVersionUpdate = item.CrossVersionUpdate
		app.Required = item.GetRequired()
		app.Status = constant.AppNormal
		apps[item.Key] = app
	}
	return apps
}

func handleErr(install model.AppInstall, err error, out string) error {
	reErr := err
	install.Message = err.Error()
	if out != "" {
		install.Message = out
		reErr = errors.New(out)
	}
	_ = appInstallRepo.Save(&install)
	return reErr
}

func getAppFromOss() error {
	res, err := http.Get(global.CONF.System.AppOss)
	if err != nil {
		return err
	}
	appByte, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return err
	}
	var ossConfig dto.AppOssConfig
	if err := json.Unmarshal(appByte, &ossConfig); err != nil {
		return err
	}
	appDir := constant.AppResourceDir
	content, _ := os.ReadFile(path.Join(appDir, "apps.json"))

	if content != nil {
		oldConfig := &dto.AppOssConfig{}
		if err := json.Unmarshal(content, oldConfig); err != nil {
			return err
		}
		if oldConfig.Version == ossConfig.Version {
			return nil
		}
	}

	fileOp := files.NewFileOp()
	if _, err := fileOp.Backup(appDir); err != nil {
		return err
	}

	packagePath := path.Join(constant.ResourceDir, path.Base(ossConfig.Package))
	if err := fileOp.DownloadFile(ossConfig.Package, packagePath); err != nil {
		return err
	}
	if err := fileOp.Decompress(packagePath, constant.ResourceDir, files.Zip); err != nil {
		return err
	}

	defer func() {
		_ = fileOp.DeleteFile(packagePath)
	}()
	return nil
}

func handleInstalled(installed []model.AppInstall) ([]dto.AppInstalled, error) {
	var res []dto.AppInstalled

	for _, installed := range installed {

		installDTO := dto.AppInstalled{
			AppInstall: installed,
		}

		app, err := appRepo.GetFirst(commonRepo.WithByID(installed.AppId))
		if err != nil {
			return nil, err
		}
		details, err := appDetailRepo.GetBy(appDetailRepo.WithAppId(app.ID))
		if err != nil {
			return nil, err
		}
		var versions []string
		for _, detail := range details {
			versions = append(versions, detail.Version)
		}
		versions = common.GetSortedVersions(versions)
		lastVersion := versions[0]

		if common.IsCrossVersion(installed.Version, lastVersion) {
			installDTO.CanUpdate = app.CrossVersionUpdate
		} else {
			installDTO.CanUpdate = common.CompareVersion(lastVersion, installed.Version)
		}
		res = append(res, installDTO)
	}

	return res, nil
}

func getAppInstallByKey(key string) (model.AppInstall, error) {
	app, err := appRepo.GetFirst(appRepo.WithKey(key))
	if err != nil {
		return model.AppInstall{}, err
	}
	appInstall, err := appInstallRepo.GetFirst(appInstallRepo.WithAppId(app.ID))
	if err != nil {
		return model.AppInstall{}, err
	}
	return appInstall, nil
}

func updateToolApp(installed model.AppInstall) {
	tooKey, ok := dto.AppToolMap[installed.App.Key]
	if !ok {
		return
	}
	toolInstall, _ := getAppInstallByKey(tooKey)
	if reflect.DeepEqual(toolInstall, model.AppInstall{}) {
		return
	}
	paramMap := make(map[string]string)
	_ = json.Unmarshal([]byte(installed.Param), &paramMap)
	envMap := make(map[string]interface{})
	_ = json.Unmarshal([]byte(toolInstall.Env), &envMap)
	if password, ok := paramMap["PANEL_DB_ROOT_PASSWORD"]; ok {
		envMap["PANEL_DB_ROOT_PASSWORD"] = password
	}
	if _, ok := envMap["PANEL_REDIS_HOST"]; ok {
		envMap["PANEL_REDIS_HOST"] = installed.ServiceName
	}
	if _, ok := envMap["PANEL_DB_HOST"]; ok {
		envMap["PANEL_DB_HOST"] = installed.ServiceName
	}

	envPath := path.Join(toolInstall.GetPath(), ".env")
	contentByte, err := json.Marshal(envMap)
	if err != nil {
		global.LOG.Errorf("update tool app [%s] error : %s", toolInstall.Name, err.Error())
		return
	}
	envFileMap := make(map[string]string)
	handleMap(envMap, envFileMap)
	if err = godotenv.Write(envFileMap, envPath); err != nil {
		global.LOG.Errorf("update tool app [%s] error : %s", toolInstall.Name, err.Error())
		return
	}
	toolInstall.Env = string(contentByte)
	if err := appInstallRepo.Save(&toolInstall); err != nil {
		global.LOG.Errorf("update tool app [%s] error : %s", toolInstall.Name, err.Error())
		return
	}
	if out, err := compose.Down(toolInstall.GetComposePath()); err != nil {
		global.LOG.Errorf("update tool app [%s] error : %s", toolInstall.Name, out)
		return
	}
	if out, err := compose.Up(toolInstall.GetComposePath()); err != nil {
		global.LOG.Errorf("update tool app [%s] error : %s", toolInstall.Name, out)
		return
	}
}
