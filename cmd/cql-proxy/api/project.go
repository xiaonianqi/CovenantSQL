/*
 * Copyright 2019 The CovenantSQL Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
	gorp "gopkg.in/gorp.v2"

	"github.com/CovenantSQL/CovenantSQL/cmd/cql-proxy/config"
	"github.com/CovenantSQL/CovenantSQL/cmd/cql-proxy/model"
	"github.com/CovenantSQL/CovenantSQL/cmd/cql-proxy/resolver"
	"github.com/CovenantSQL/CovenantSQL/cmd/cql-proxy/storage"
	"github.com/CovenantSQL/CovenantSQL/conf"
	"github.com/CovenantSQL/CovenantSQL/crypto/asymmetric"
	"github.com/CovenantSQL/CovenantSQL/proto"
	"github.com/CovenantSQL/CovenantSQL/types"
	"github.com/CovenantSQL/CovenantSQL/utils/log"
)

const (
	metaTableUserInfo      = "____user"
	metaTableProjectConfig = "____config"
	metaTableSession       = "____session"
	deletedTablePrefix     = "____deleted"
)

type projectRulesContext struct {
	dbID   proto.DatabaseID
	db     *gorp.DbMap
	group  *model.ProjectConfig
	tables map[string]*model.ProjectConfig

	toUpdate *model.ProjectConfig
	toInsert *model.ProjectConfig
}

func getProjects(c *gin.Context) {
	developer := getDeveloperID(c)
	p, err := model.GetMainAccount(model.GetDB(c), developer)
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusForbidden, ErrNoMainAccount)
		return
	}

	projectList, err := model.GetProjects(model.GetDB(c), developer, p.ID)
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusForbidden, ErrGetProjectsFailed)
		return
	}

	accountAddr, err := p.Account.Get()
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusBadRequest, ErrParseAccountFailed)
		return
	}

	var apiResp []gin.H

	for _, p := range projectList {
		var (
			profile *types.SQLChainProfile
			balance gin.H
		)

		if profile, err = getDatabaseProfile(p.DB); err != nil {
			_ = c.Error(err)
			abortWithError(c, http.StatusInternalServerError, ErrSendETLSRPCFailed)
			return
		}

		for _, user := range profile.Users {
			if user.Address == accountAddr {
				balance = gin.H{
					"deposit":         user.Deposit,
					"arrears":         user.Arrears,
					"advance_payment": user.AdvancePayment,
				}
				break
			}
		}

		apiResp = append(apiResp, gin.H{
			"id":      p.ID,
			"project": p.DB,
			"alias":   p.Alias,
			"balance": balance,
		})
	}

	responseWithData(c, http.StatusOK, gin.H{
		"projects": apiResp,
	})
}

func createProject(c *gin.Context) {
	r := struct {
		NodeCount uint16 `json:"node" form:"node" binding:"gt=0"`
	}{}

	if err := c.ShouldBind(&r); err != nil {
		abortWithError(c, http.StatusBadRequest, err)
		return
	}

	developer := getDeveloperID(c)
	p, err := model.GetMainAccount(model.GetDB(c), developer)
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusBadRequest, ErrNoMainAccount)
		return
	}

	// run task
	taskID, err := getTaskManager(c).New(model.TaskCreateProject, developer, p.ID, gin.H{
		"node_count": r.NodeCount,
	})
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusInternalServerError, ErrCreateTaskFailed)
		return
	}

	responseWithData(c, http.StatusOK, gin.H{
		"task_id": taskID,
	})
}

// CreateProjectTask handles the project creation process.
func CreateProjectTask(ctx context.Context, cfg *config.Config, db *gorp.DbMap, t *model.Task) (r gin.H, err error) {
	args := struct {
		NodeCount uint16 `json:"node_count"`
	}{}

	err = json.Unmarshal(t.RawArgs, &args)
	if err != nil {
		err = errors.Wrapf(err, "unmarshal task args failed")
		return
	}

	tx, dbID, key, err := createDatabase(db, t.Developer, t.Account, args.NodeCount)
	if err != nil {
		err = errors.Wrapf(err, "create database failed")
		return
	}

	// wait for transaction to complete in several cycles
	timeoutCtx, cancelCtx := context.WithTimeout(ctx, 3*time.Minute)
	defer cancelCtx()

	lastState, err := waitForTxState(timeoutCtx, tx)
	if err != nil {
		err = errors.Wrapf(err, "wait for database creation confirmation failed")
		r = gin.H{
			"project": dbID,
			"db":      dbID,
			"tx":      tx.String(),
			"state":   lastState.String(),
		}

		return
	}

	// wait for projectDB to ready deployed
	time.Sleep(30 * time.Second)

	_, err = initProjectDB(dbID, key)
	if err != nil {
		err = errors.Wrapf(err, "init project database meta tables failed")
		return
	}

	// bind database to current developer
	_, err = model.AddProject(db, dbID, t.Developer, t.Account)
	if err != nil {
		err = errors.Wrapf(err, "register project failed")
	}

	r = gin.H{
		"tx":      tx.String(),
		"state":   lastState.String(),
		"project": dbID,
		"db":      dbID,
	}

	return
}

func projectUserList(c *gin.Context) {
	r := struct {
		DB              proto.DatabaseID `json:"db" json:"project" form:"db" form:"project" uri:"db" uri:"project" binding:"required,len=64"`
		Term            string           `json:"term" form:"term" binding:"max=32"`
		ShowOnlyEnabled bool             `json:"enabled" form:"enabled"`
		Offset          int64            `json:"offset" form:"offset" binding:"gte=0"`
		Limit           int64            `json:"limit" form:"limit" binding:"gte=0"`
	}{}

	_ = c.ShouldBindUri(&r)

	if err := c.ShouldBind(&r); err != nil {
		abortWithError(c, http.StatusBadRequest, err)
		return
	}

	if r.Limit == 0 {
		r.Limit = 20
	}

	_, projectDB, err := getProjectDB(c, r.DB)
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusForbidden, ErrLoadProjectDatabaseFailed)
		return
	}

	users, total, err := model.GetProjectUserList(projectDB, r.Term, r.ShowOnlyEnabled, r.Offset, r.Limit)
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusInternalServerError, ErrGetUserListFailed)
		return
	}

	var resp []gin.H

	for _, u := range users {
		resp = append(resp, gin.H{
			"id":           u.ID,
			"name":         u.Name,
			"email":        u.Email,
			"state":        u.State.String(),
			"provider":     u.Provider,
			"provider_uid": u.ProviderUID,
			"extra":        u.Extra,
			"created":      formatUnixTime(u.Created),
			"last_login":   formatUnixTime(u.LastLogin),
		})
	}

	responseWithData(c, http.StatusOK, gin.H{
		"users": resp,
		"total": total,
	})
}

func preRegisterProjectUser(c *gin.Context) {
	r := struct {
		DB       proto.DatabaseID `json:"db" json:"project" form:"db" form:"project" uri:"db" uri:"project" binding:"required,len=64"`
		Name     string           `json:"name" form:"name"`
		Email    string           `json:"email" form:"email" binding:"required,email"`
		Provider string           `json:"provider" form:"provider" binding:"required"`
	}{}

	_ = c.ShouldBindUri(&r)

	if err := c.ShouldBind(&r); err != nil {
		abortWithError(c, http.StatusBadRequest, err)
		return
	}

	_, projectDB, err := getProjectDB(c, r.DB)
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusForbidden, ErrLoadProjectDatabaseFailed)
		return
	}

	var u *model.ProjectUser
	u, err = model.PreRegisterUser(projectDB, r.Provider, r.Name, r.Email)
	if err != nil {
		_ = c.Error(err)

		if strings.Contains(err.Error(), "constraint failed") {
			err = ErrPreRegisterUserAlreadyExists
		} else {
			err = ErrPreRegisterUserFailed
		}

		abortWithError(c, http.StatusInternalServerError, err)
		return
	}

	responseWithData(c, http.StatusOK, gin.H{
		"name":     u.Name,
		"email":    u.Email,
		"provider": u.Provider,
		"project":  r.DB,
	})
}

func queryProjectUser(c *gin.Context) {
	r := struct {
		DB proto.DatabaseID `json:"db" json:"project" form:"db" form:"project" uri:"db" uri:"project" binding:"required,len=64"`
		ID int64            `json:"id" form:"id" uri:"id" binding:"required,gt=0"`
	}{}

	_ = c.ShouldBindUri(&r)

	if err := c.ShouldBind(&r); err != nil {
		abortWithError(c, http.StatusBadRequest, err)
		return
	}

	_, projectDB, err := getProjectDB(c, r.DB)
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusForbidden, ErrLoadProjectDatabaseFailed)
		return
	}

	u, err := model.GetProjectUser(projectDB, r.ID)
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusInternalServerError, ErrGetProjectUserFailed)
		return
	}

	responseWithData(c, http.StatusOK, gin.H{
		"id":           u.ID,
		"name":         u.Name,
		"email":        u.Email,
		"state":        u.State.String(),
		"provider":     u.Provider,
		"provider_uid": u.ProviderUID,
		"extra":        u.Extra,
		"created":      formatUnixTime(u.Created),
		"last_login":   formatUnixTime(u.LastLogin),
	})
}

func updateProjectUser(c *gin.Context) {
	r := struct {
		DB       proto.DatabaseID `json:"db" json:"project" form:"db" form:"project" uri:"db" uri:"project" binding:"required,len=64"`
		ID       int64            `json:"id" form:"id" uri:"id" binding:"required,gt=0"`
		Name     string           `json:"name" form:"name"`
		Email    string           `json:"email" form:"email" binding:"omitempty,email"`
		Provider string           `json:"provider" form:"provider"`
		State    string           `json:"state" form:"state"`
	}{}

	_ = c.ShouldBindUri(&r)

	if err := c.ShouldBind(&r); err != nil {
		abortWithError(c, http.StatusBadRequest, err)
		return
	}

	_, projectDB, err := getProjectDB(c, r.DB)
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusForbidden, ErrLoadProjectDatabaseFailed)
		return
	}

	u, err := model.GetProjectUser(projectDB, r.ID)
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusInternalServerError, ErrGetProjectUserFailed)
		return
	}

	if r.Name != "" {
		u.Name = r.Name
	}
	if r.Email != "" {
		u.Email = r.Email
	}
	if r.Provider != "" {
		u.Provider = r.Provider
	}
	if r.State != "" {
		u.State, err = model.ParseProjectUserState(r.State)
		if err != nil {
			_ = c.Error(err)
			abortWithError(c, http.StatusBadRequest, ErrGetProjectUserStateFailed)
			return
		}
	}

	err = model.UpdateProjectUser(projectDB, u)
	if err != nil {
		_ = c.Error(err)
		if strings.Contains(err.Error(), "constraint failed") {
			err = ErrUpdateUserConflictWithExisting
		} else {
			err = ErrUpdateUserFailed
		}
		abortWithError(c, http.StatusInternalServerError, err)
		return
	}

	responseWithData(c, http.StatusOK, gin.H{
		"id":           u.ID,
		"name":         u.Name,
		"email":        u.Email,
		"state":        u.State.String(),
		"provider":     u.Provider,
		"provider_uid": u.ProviderUID,
		"extra":        u.Extra,
		"created":      formatUnixTime(u.Created),
		"last_login":   formatUnixTime(u.LastLogin),
	})
}

func getProjectOAuthCallback(c *gin.Context) {
	r := struct {
		DB       proto.DatabaseID `json:"db" json:"project" form:"db" form:"project" uri:"db" uri:"project" binding:"required,len=64"`
		Provider string           `json:"provider" form:"provider" uri:"provider" binding:"required,max=256"`
	}{}

	_ = c.ShouldBindUri(&r)

	if err := c.ShouldBind(&r); err != nil {
		abortWithError(c, http.StatusBadRequest, err)
		return
	}

	developer := getDeveloperID(c)

	p, err := model.GetProjectByID(model.GetDB(c), r.DB, developer)
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusForbidden, ErrGetProjectFailed)
		return
	}

	cfg := getConfig(c)
	if cfg == nil || len(cfg.Hosts) == 0 {
		abortWithError(c, http.StatusInternalServerError, ErrNoPublicServiceHosts)
		return
	}

	var (
		authorize []string
		callback  []string
	)

	for _, h := range cfg.Hosts {
		// project alias happy and host api.covenantsql.io will produce happy.api.covenantsql.io as service host
		authorize = append(authorize,
			fmt.Sprintf("http://%s.%s/auth/authorize/%s", p.Alias, strings.TrimLeft(h, "."), r.Provider))
		callback = append(callback,
			fmt.Sprintf("http://%s.%s/auth/callback/%s", p.Alias, strings.TrimLeft(h, "."), r.Provider))
	}

	responseWithData(c, http.StatusOK, gin.H{
		"authorizes": authorize,
		"callbacks":  callback,
	})
}

func updateProjectOAuthConfig(c *gin.Context) {
	r := struct {
		DB       proto.DatabaseID `json:"db" json:"project" form:"db" form:"project" uri:"db" uri:"project" binding:"required,len=64"`
		Provider string           `json:"provider" form:"provider" uri:"provider" binding:"required,max=256"`
		model.ProjectOAuthConfig
		// additional parameters, see ProjectOAuthConfig structure
	}{}

	_ = c.ShouldBindUri(&r)

	if err := c.ShouldBind(&r); err != nil {
		abortWithError(c, http.StatusBadRequest, err)
		return
	}

	_, projectDB, err := getProjectDB(c, r.DB)
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusForbidden, ErrLoadProjectDatabaseFailed)
		return
	}

	cfg := &r.ProjectOAuthConfig

	if cfg.ClientID == "" && cfg.ClientSecret == "" {
		// update nothing
		_ = c.Error(err)
		abortWithError(c, http.StatusBadRequest, ErrIncompleteOAuthConfig)
		return
	}

	var (
		p   *model.ProjectConfig
		poc *model.ProjectOAuthConfig
	)

	p, poc, err = model.GetProjectOAuthConfig(projectDB, r.Provider)
	if err != nil {
		// not exists, create
		if cfg.ClientID == "" || cfg.ClientSecret == "" {
			abortWithError(c, http.StatusBadRequest, ErrIncompleteOAuthConfig)
			return
		}
		p, err = model.AddProjectConfig(projectDB, model.ProjectConfigOAuth, r.Provider, cfg)
		if err != nil {
			_ = c.Error(err)
			abortWithError(c, http.StatusInternalServerError, ErrAddProjectOAuthConfigFailed)
			return
		}
		poc = cfg
	} else {
		// update config
		if cfg.ClientID != "" {
			poc.ClientID = cfg.ClientID
		}
		if cfg.ClientSecret != "" {
			poc.ClientSecret = cfg.ClientSecret
		}
		if cfg.Enabled != nil {
			poc.Enabled = cfg.Enabled
		}
		err = model.UpdateProjectConfig(projectDB, p)
		if err != nil {
			_ = c.Error(err)
			abortWithError(c, http.StatusInternalServerError, ErrUpdateProjectConfigFailed)
			return
		}
	}

	responseWithData(c, http.StatusOK, gin.H{
		"oauth": gin.H{
			"provider": r.Provider,
			"config":   poc,
		},
	})
}

func updateProjectGroupConfig(c *gin.Context) {
	r := struct {
		DB proto.DatabaseID `json:"db" json:"project" form:"db" form:"project" uri:"db" uri:"project" binding:"required,len=64"`
		model.ProjectGroupConfig
		// additional parameters, see ProjectGroupConfig structure
	}{}

	_ = c.ShouldBindUri(&r)

	if err := c.ShouldBind(&r); err != nil {
		abortWithError(c, http.StatusBadRequest, err)
		return
	}

	_, projectDB, err := getProjectDB(c, r.DB)
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusForbidden, ErrLoadProjectDatabaseFailed)
		return
	}

	rulesCtx, err := getRulesContext(r.DB, projectDB)
	if err != nil {
		// get rules context failed
		_ = c.Error(err)
		abortWithError(c, http.StatusInternalServerError, ErrGetProjectRulesFailed)
		return
	}

	if rulesCtx.group == nil {
		rulesCtx.group = &model.ProjectConfig{
			Type:  model.ProjectConfigGroup,
			Key:   "",
			Value: &r.ProjectGroupConfig,
		}
		rulesCtx.toInsert = rulesCtx.group
	} else {
		rulesCtx.group.Value = &r.ProjectGroupConfig
		rulesCtx.toUpdate = rulesCtx.group
	}

	if _, err = populateRulesContext(c, rulesCtx); err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusBadRequest, ErrPopulateProjectRulesFailed)
		return
	}

	responseWithData(c, http.StatusOK, gin.H{
		"group": rulesCtx.group.Value,
	})
}

func updateProjectMiscConfig(c *gin.Context) {
	r := struct {
		DB proto.DatabaseID `json:"db" json:"project" form:"db" form:"project" uri:"db" uri:"project" binding:"required,len=64"`
		model.ProjectMiscConfig
		// additional parameters, see ProjectMiscConfig structure
	}{}

	_ = c.ShouldBindUri(&r)

	if err := c.ShouldBind(&r); err != nil {
		abortWithError(c, http.StatusBadRequest, err)
		return
	}

	_, projectDB, err := getProjectDB(c, r.DB)
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusForbidden, ErrLoadProjectDatabaseFailed)
		return
	}

	cfg := &r.ProjectMiscConfig

	// alias goes to project config, also set backup to project database
	if cfg.Alias != "" {
		// set alias to project database
		err = model.SetProjectAlias(model.GetDB(c), r.DB, getDeveloperID(c), cfg.Alias)
		if err != nil {
			_ = c.Error(err)
			abortWithError(c, http.StatusInternalServerError, ErrSetProjectAliasFailed)
			return
		}
	}

	// other goes to config in project database
	var (
		p   *model.ProjectConfig
		pmc *model.ProjectMiscConfig
	)
	p, pmc, err = model.GetProjectMiscConfig(projectDB)
	if err != nil {
		// not exists, create
		p, err = model.AddProjectConfig(projectDB, model.ProjectConfigMisc, "", cfg)
		if err != nil {
			_ = c.Error(err)
			abortWithError(c, http.StatusInternalServerError, ErrAddProjectMiscConfigFailed)
			return
		}
		pmc = cfg
	} else {
		if cfg.Alias != "" {
			pmc.Alias = cfg.Alias
		}
		if cfg.Enabled != nil {
			pmc.Enabled = cfg.Enabled
		}
		if cfg.EnableSignUp != nil {
			pmc.EnableSignUp = cfg.EnableSignUp
		}
		if cfg.EnableSignUpVerification != nil {
			pmc.EnableSignUpVerification = cfg.EnableSignUpVerification
		}
		err = model.UpdateProjectConfig(projectDB, p)
		if err != nil {
			_ = c.Error(err)
			abortWithError(c, http.StatusInternalServerError, ErrUpdateProjectConfigFailed)
			return
		}
	}

	responseWithData(c, http.StatusOK, gin.H{
		"misc": pmc,
	})
}

func getProjectConfig(c *gin.Context) {
	// get all configs including tables/oauth config
	r := struct {
		DB proto.DatabaseID `json:"db" json:"project" form:"db" form:"project" uri:"db" uri:"project" binding:"required,len=64"`
	}{}

	_ = c.ShouldBindUri(&r)

	if err := c.ShouldBind(&r); err != nil {
		abortWithError(c, http.StatusBadRequest, err)
		return
	}

	projectData, projectDB, err := getProjectDB(c, r.DB)
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusForbidden, ErrLoadProjectDatabaseFailed)
		return
	}

	projectConfigList, err := model.GetAllProjectConfig(projectDB)
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusInternalServerError, ErrGetProjectConfigFailed)
		return
	}

	var (
		miscConfig   interface{}
		groupConfig  interface{}
		oauthConfig  []gin.H
		tablesConfig []gin.H
	)
	for _, p := range projectConfigList {
		switch p.Type {
		case model.ProjectConfigMisc:
			miscConfig = p.Value
		case model.ProjectConfigOAuth:
			oauthConfig = append(oauthConfig, gin.H{
				"provider": p.Key,
				"config":   p.Value,
			})
		case model.ProjectConfigTable:
			tablesConfig = append(tablesConfig, gin.H{
				"table":  p.Key,
				"config": p.Value,
			})
		case model.ProjectConfigGroup:
			groupConfig = p.Value
		}
	}

	if miscConfig == nil {
		miscConfig = map[string]interface{}{}
	} else if mc, ok := miscConfig.(*model.ProjectMiscConfig); !ok || mc == nil {
		miscConfig = map[string]interface{}{}
	}

	var (
		resp = gin.H{
			"misc":               miscConfig,
			"oauth":              oauthConfig,
			"tables":             tablesConfig,
			"group":              groupConfig,
			"client_api_domains": nil,
		}
		cfg = getConfig(c)
	)

	if cfg != nil && len(cfg.Hosts) > 0 {
		var domains []string

		for _, h := range cfg.Hosts {
			apiHost := projectData.Alias + "." + strings.TrimLeft(h, ".")
			domains = append(domains, apiHost)
		}

		resp["client_api_domains"] = domains
	}

	responseWithData(c, http.StatusOK, resp)
}

func getProjectTables(c *gin.Context) {
	r := struct {
		DB proto.DatabaseID `json:"db" json:"project" form:"db" form:"project" uri:"db" uri:"project" binding:"required,len=64"`
	}{}

	_ = c.ShouldBindUri(&r)

	if err := c.ShouldBind(&r); err != nil {
		abortWithError(c, http.StatusBadRequest, err)
		return
	}

	_, projectDB, err := getProjectDB(c, r.DB)
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusForbidden, ErrLoadProjectDatabaseFailed)
		return
	}

	tables, err := model.GetProjectTablesName(projectDB)
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusForbidden, ErrGetProjectTableNamesFailed)
		return
	}

	responseWithData(c, http.StatusOK, gin.H{
		"tables": tables,
	})
}

func createProjectTable(c *gin.Context) {
	r := struct {
		DB            proto.DatabaseID `json:"db" json:"project" form:"db" form:"project" uri:"db" uri:"project" binding:"required,len=64"`
		Table         string           `json:"table" form:"table" uri:"table" binding:"required,max=128"`
		ColumnNames   []string         `json:"names" form:"names" binding:"required,min=1,dive,required,max=32"`
		ColumnTypes   []string         `json:"types" form:"types" binding:"required,min=1,dive,required,max=16"`
		PrimaryKey    string           `json:"primary_key" form:"primary_key" binding:"omitempty,max=32"`
		AutoIncrement bool             `json:"auto_increment" form:"auto_increment"`
	}{}

	_ = c.ShouldBindUri(&r)

	if err := c.ShouldBind(&r); err != nil {
		abortWithError(c, http.StatusBadRequest, err)
		return
	}

	if len(r.ColumnNames) != len(r.ColumnTypes) {
		abortWithError(c, http.StatusBadRequest, ErrMismatchedColumnNamesAndTypes)
		return
	}

	if strings.EqualFold(r.Table, metaTableProjectConfig) || strings.EqualFold(r.Table, metaTableUserInfo) ||
		strings.EqualFold(r.Table, metaTableSession) || strings.HasPrefix(r.Table, deletedTablePrefix) {
		abortWithError(c, http.StatusBadRequest, ErrReservedTableName)
		return
	}

	// try find primary key in columns
	pkIdx := -1

	if r.PrimaryKey != "" {
		for idx, colName := range r.ColumnNames {
			if strings.EqualFold(colName, r.PrimaryKey) {
				pkIdx = idx

				if r.AutoIncrement &&
					!strings.EqualFold(r.ColumnTypes[idx], "NUMBER") &&
					!strings.EqualFold(r.ColumnTypes[idx], "INTEGER") {
					abortWithError(c, http.StatusBadRequest, ErrInvalidAutoIncrementColumnType)
					return
				}

				break
			}
		}

		if pkIdx == -1 {
			abortWithError(c, http.StatusBadRequest, ErrUnknownPrimaryKeyColumn)
			return
		}
	}

	_, projectDB, err := getProjectDB(c, r.DB)
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusForbidden, ErrLoadProjectDatabaseFailed)
		return
	}

	// create table in project db
	// build create table sql
	sql := `CREATE TABLE "` + r.Table + `" (` + "\n"

	for idx, colName := range r.ColumnNames {
		if idx != 0 {
			sql += ",\n"
		}

		// remap column types
		var colType string

		switch strings.ToUpper(r.ColumnTypes[idx]) {
		case "NUMBER":
			if r.AutoIncrement {
				colType = "INTEGER"
			} else {
				colType = "NUMERIC"
			}
		case "TEXT":
			colType = "TEXT"
		case "BINARY":
			colType = "BLOB"
		default:
			colType = "NUMERIC"
		}

		sql += fmt.Sprintf(`"%s" %s`, colName, colType)
		if idx == pkIdx {
			sql += " PRIMARY KEY "

			if r.AutoIncrement {
				sql += " AUTOINCREMENT "
			}
		}
	}

	sql += `);`

	_, err = projectDB.Exec(sql)
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusInternalServerError, ErrDDLExecuteFailed)
		return
	}

	// save project table meta config
	ptc := &model.ProjectTableConfig{
		Columns:       r.ColumnNames,
		Types:         r.ColumnTypes,
		PrimaryKey:    r.PrimaryKey,
		AutoIncrement: r.AutoIncrement,
	}
	p, err := model.AddProjectConfig(projectDB, model.ProjectConfigTable, r.Table, ptc)
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusInternalServerError, ErrAddProjectTableConfigFailed)
		return
	}

	responseWithData(c, http.StatusOK, gin.H{
		"project":      r.DB,
		"db":           r.DB,
		"table":        r.Table,
		"columns":      r.ColumnNames,
		"types":        r.ColumnTypes,
		"created":      formatUnixTime(p.Created),
		"last_updated": formatUnixTime(p.LastUpdated),
		"keys":         ptc.Keys,
		"rules":        ptc.Rules,
		"is_deleted":   ptc.IsDeleted,
	})
}

func getProjectTableDetail(c *gin.Context) {
	r := struct {
		DB    proto.DatabaseID `json:"db" json:"project" form:"db" form:"project" uri:"db" uri:"project" binding:"required,len=64"`
		Table string           `json:"table" form:"table" uri:"table" binding:"required,max=128"`
	}{}

	_ = c.ShouldBindUri(&r)

	if err := c.ShouldBind(&r); err != nil {
		abortWithError(c, http.StatusBadRequest, err)
		return
	}

	_, projectDB, err := getProjectDB(c, r.DB)
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusForbidden, ErrLoadProjectDatabaseFailed)
		return
	}

	pc, ptc, err := model.GetProjectTableConfig(projectDB, r.Table)
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusInternalServerError, ErrGetProjectTableConfigFailed)
		return
	}

	ddl, err := projectDB.SelectStr(`SHOW CREATE TABLE "` + r.Table + `"`)
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusInternalServerError, ErrGetProjectTableDDLFailed)
		return
	}

	responseWithData(c, http.StatusOK, gin.H{
		"project":      r.DB,
		"db":           r.DB,
		"table":        r.Table,
		"created":      formatUnixTime(pc.Created),
		"last_updated": formatUnixTime(pc.LastUpdated),
		"columns":      ptc.Columns,
		"types":        ptc.Types,
		"keys":         ptc.Keys,
		"rules":        ptc.Rules,
		"is_deleted":   ptc.IsDeleted,
		"ddl":          ddl,
	})
}

func addFieldsToProjectTable(c *gin.Context) {
	r := struct {
		DB         proto.DatabaseID `json:"db" json:"project" form:"db" form:"project" uri:"db" uri:"project" binding:"required,len=64"`
		Table      string           `json:"table" form:"table" uri:"table" binding:"required,max=128"`
		ColumnName string           `json:"name" form:"name" binding:"required,max=32"`
		ColumnType string           `json:"type" form:"type" binding:"required,max=16"`
	}{}

	_ = c.ShouldBindUri(&r)

	if err := c.ShouldBind(&r); err != nil {
		abortWithError(c, http.StatusBadRequest, err)
		return
	}

	_, projectDB, err := getProjectDB(c, r.DB)
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusForbidden, ErrLoadProjectDatabaseFailed)
		return
	}

	pc, ptc, err := model.GetProjectTableConfig(projectDB, r.Table)
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusInternalServerError, ErrGetProjectTableConfigFailed)
		return
	}

	// find column in current column list
	for _, col := range ptc.Columns {
		if strings.EqualFold(col, r.ColumnName) {
			_ = c.Error(err)
			abortWithError(c, http.StatusBadRequest, ErrColumnAlreadyExists)
			return
		}
	}

	ptc.Columns = append(ptc.Columns, r.ColumnName)
	ptc.Types = append(ptc.Types, r.ColumnType)

	// execute alter table add column to database
	_, err = projectDB.Exec(fmt.Sprintf(
		`ALTER TABLE "%s" ADD COLUMN "%s" %s;`, r.Table, r.ColumnName, r.ColumnType))
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusInternalServerError, ErrDDLExecuteFailed)
		return
	}

	err = model.UpdateProjectConfig(projectDB, pc)
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusInternalServerError, ErrUpdateProjectConfigFailed)
		return
	}

	responseWithData(c, http.StatusOK, gin.H{
		"project":      r.DB,
		"db":           r.DB,
		"table":        r.Table,
		"created":      formatUnixTime(pc.Created),
		"last_updated": formatUnixTime(pc.LastUpdated),
		"columns":      ptc.Columns,
		"types":        ptc.Types,
		"keys":         ptc.Keys,
		"rules":        ptc.Rules,
		"is_deleted":   ptc.IsDeleted,
	})
}

func dropProjectTable(c *gin.Context) {
	r := struct {
		DB    proto.DatabaseID `json:"db" json:"project" form:"db" form:"project" uri:"db" uri:"project" binding:"required,len=64"`
		Table string           `json:"table" form:"table" uri:"table" binding:"required,max=128"`
	}{}

	_ = c.ShouldBindUri(&r)

	if err := c.ShouldBind(&r); err != nil {
		abortWithError(c, http.StatusBadRequest, err)
		return
	}

	_, projectDB, err := getProjectDB(c, r.DB)
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusForbidden, ErrLoadProjectDatabaseFailed)
		return
	}

	pc, ptc, err := model.GetProjectTableConfig(projectDB, r.Table)
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusInternalServerError, ErrGetProjectTableConfigFailed)
		return
	}

	if ptc.IsDeleted {
		_ = c.Error(err)
		abortWithError(c, http.StatusNotFound, ErrTableNotExists)
		return
	}

	// rename table
	newName := fmt.Sprintf("%s_%s_%d", deletedTablePrefix, r.Table, pc.ID)
	_, err = projectDB.Exec(fmt.Sprintf(
		`ALTER TABLE "%s" RENAME TO "%s"`, r.Table, newName))
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusInternalServerError, ErrDDLExecuteFailed)
		return
	}

	ptc.IsDeleted = true

	err = model.UpdateProjectConfig(projectDB, pc)
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusInternalServerError, ErrUpdateProjectConfigFailed)
		return
	}

	responseWithData(c, http.StatusOK, gin.H{
		"project":      r.DB,
		"db":           r.DB,
		"table":        r.Table,
		"created":      formatUnixTime(pc.Created),
		"last_updated": formatUnixTime(pc.LastUpdated),
		"columns":      ptc.Columns,
		"types":        ptc.Types,
		"keys":         ptc.Keys,
		"rules":        ptc.Rules,
		"is_deleted":   ptc.IsDeleted,
	})
}

func batchQueryProjectUser(c *gin.Context) {
	r := struct {
		DB proto.DatabaseID `json:"db" json:"project" form:"db" form:"project" uri:"db" uri:"project" binding:"required,len=64"`
		ID []int64          `json:"id" form:"id" binding:"required,dive,required,gt=0"`
	}{}

	_ = c.ShouldBindUri(&r)

	if err := c.ShouldBind(&r); err != nil {
		abortWithError(c, http.StatusBadRequest, err)
		return
	}

	_, projectDB, err := getProjectDB(c, r.DB)
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusForbidden, ErrLoadProjectDatabaseFailed)
		return
	}

	users, err := model.GetProjectUsers(projectDB, r.ID)
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusInternalServerError, ErrGetProjectUserFailed)
		return
	}

	var resp = gin.H{}

	for _, u := range users {
		resp[fmt.Sprint(u.ID)] = gin.H{
			"id":           u.ID,
			"name":         u.Name,
			"email":        u.Email,
			"state":        u.State.String(),
			"provider":     u.Provider,
			"provider_uid": u.ProviderUID,
			"extra":        u.Extra,
			"created":      formatUnixTime(u.Created),
			"last_login":   formatUnixTime(u.LastLogin),
		}
	}

	responseWithData(c, http.StatusOK, gin.H{
		"users": resp,
	})
}

func updateProjectTableRules(c *gin.Context) {
	r := struct {
		DB    proto.DatabaseID `json:"db" json:"project" form:"db" form:"project" uri:"db" uri:"project" binding:"required,len=64"`
		Table string           `json:"table" form:"table" uri:"table" binding:"required,max=128"`
		Rules json.RawMessage  `json:"rules"`
	}{}

	_ = c.ShouldBindUri(&r)

	if err := c.ShouldBind(&r); err != nil {
		abortWithError(c, http.StatusBadRequest, err)
		return
	}

	_, projectDB, err := getProjectDB(c, r.DB)
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusForbidden, ErrLoadProjectDatabaseFailed)
		return
	}

	rulesCtx, err := getRulesContext(r.DB, projectDB)
	if err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusInternalServerError, ErrGetProjectRulesFailed)
		return
	}

	var (
		pc  *model.ProjectConfig
		ptc *model.ProjectTableConfig
		ok  bool
	)

	if pc, ok = rulesCtx.tables[r.Table]; ok {
		ptc = pc.Value.(*model.ProjectTableConfig)
		ptc.Rules = r.Rules
		rulesCtx.toUpdate = pc
	} else {
		_ = c.Error(err)
		abortWithError(c, http.StatusInternalServerError, ErrTableNotExists)
		return
	}

	if _, err = populateRulesContext(c, rulesCtx); err != nil {
		_ = c.Error(err)
		abortWithError(c, http.StatusBadRequest, ErrPopulateProjectRulesFailed)
		return
	}

	responseWithData(c, http.StatusOK, gin.H{
		"project":      r.DB,
		"db":           r.DB,
		"table":        r.Table,
		"created":      formatUnixTime(pc.Created),
		"last_updated": formatUnixTime(pc.LastUpdated),
		"columns":      ptc.Columns,
		"types":        ptc.Types,
		"keys":         ptc.Keys,
		"rules":        ptc.Rules,
		"is_deleted":   ptc.IsDeleted,
	})
}

func getProjectAudits(c *gin.Context) {

}

func initProjectDB(dbID proto.DatabaseID, key *asymmetric.PrivateKey) (db *gorp.DbMap, err error) {
	nodeID, err := getDatabaseLeaderNodeID(dbID)
	if err != nil {
		err = errors.Wrapf(err, "could not get project leader node")
		return
	}

	db = storage.NewImpersonatedDB(
		conf.GConf.ThisNodeID,
		getNodePCaller(nodeID),
		dbID,
		key,
	)

	tblUser := db.AddTableWithName(model.ProjectUser{}, metaTableUserInfo).
		SetKeys(true, "ID")
	tblUser.AddIndex("____idx_user_1", "", []string{"provider", "email"}).SetUnique(true)
	tblConfig := db.AddTableWithName(model.ProjectConfig{}, metaTableProjectConfig).
		SetKeys(true, "ID")
	tblConfig.AddIndex("____idx_config_1", "", []string{"type", "key"}).SetUnique(true)
	db.AddTableWithName(model.Session{}, metaTableSession).SetKeys(false, "ID")

	err = db.CreateTablesIfNotExists()

	// ignore index error
	_ = db.CreateIndex()

	if log.GetLevel() == log.DebugLevel {
		db.TraceOn(string(dbID), log.StandardLogger())
	}

	return
}

func getProjectDB(c *gin.Context, dbID proto.DatabaseID) (project *model.Project, db *gorp.DbMap, err error) {
	developer := getDeveloperID(c)

	project, err = model.GetProjectByID(model.GetDB(c), dbID, developer)
	if err != nil {
		err = errors.Wrapf(err, "get project info failed")
		return
	}

	p, err := model.GetAccountByID(model.GetDB(c), developer, project.Account)
	if err != nil {
		err = errors.Wrapf(err, "get project owner user info failed")
		return
	}

	if err = p.LoadPrivateKey(); err != nil {
		err = errors.Wrapf(err, "decode account private key failed")
		return
	}

	db, err = initProjectDB(dbID, p.Key)
	if err != nil {
		err = errors.Wrapf(err, "init project database failed")
	}

	return
}

func getRulesContext(dbID proto.DatabaseID, db *gorp.DbMap) (ctx *projectRulesContext, err error) {
	ctx = &projectRulesContext{
		dbID:   dbID,
		db:     db,
		tables: map[string]*model.ProjectConfig{},
	}

	var configs []*model.ProjectConfig
	configs, err = model.GetAllProjectConfig(db)
	if err != nil {
		err = errors.Wrapf(err, "get project config failed")
		return
	}

	for _, cfg := range configs {
		switch cfg.Type {
		case model.ProjectConfigTable:
			ctx.tables[cfg.Key] = cfg
		case model.ProjectConfigGroup:
			ctx.group = cfg
		}
	}

	return
}

func populateRulesContext(c *gin.Context, ctx *projectRulesContext) (r *resolver.Rules, err error) {
	var (
		rm         = getRulesManager(c)
		groupRules = map[string][]string{}
		tableRules = map[string]json.RawMessage{}
	)

	if ctx.group != nil {
		gc := ctx.group.Value.(*model.ProjectGroupConfig)
		for groupName, userIDs := range gc.Groups {
			for _, userID := range userIDs {
				groupRules[groupName] = append(groupRules[groupName], fmt.Sprint(userID))
			}
		}
	}

	for tableName, ptc := range ctx.tables {
		tableRule := ptc.Value.(*model.ProjectTableConfig).Rules

		// treat all empty values as nil
		if len(tableRule) > 0 &&
			!bytes.Equal(tableRule, []byte("null")) &&
			!bytes.HasPrefix(tableRule, []byte{'{'}) {
			switch string(tableRule) {
			case "0", "false", `""`:
				tableRules[tableName] = nil
				continue
			}
		}

		tableRules[tableName] = tableRule
	}

	r, err = resolver.CompileRules(map[string]interface{}{
		"groups": groupRules,
		"rules":  tableRules,
	})
	if err != nil {
		err = errors.Wrapf(err, "compile rules failed")
		return
	}

	if ctx.toUpdate != nil {
		err = model.UpdateProjectConfig(ctx.db, ctx.toUpdate)
		if err != nil {
			err = errors.Wrapf(err, "execute rules config update failed")
			return
		}
	}

	if ctx.toInsert != nil {
		err = model.AddRawProjectConfig(ctx.db, ctx.toInsert)
		if err != nil {
			err = errors.Wrapf(err, "execute new rules creation failed")
			return
		}
	}

	rm.Set(ctx.dbID, r)

	return
}

func loadRules(c *gin.Context, dbID proto.DatabaseID, db *gorp.DbMap) (r *resolver.Rules, err error) {
	rm := getRulesManager(c)

	r = rm.Get(dbID)
	if r == nil {
		var ctx *projectRulesContext
		ctx, err = getRulesContext(dbID, db)
		if err != nil {
			err = errors.Wrapf(err, "get rules failed")
			return
		}

		r, err = populateRulesContext(c, ctx)
		if err != nil {
			err = errors.Wrapf(err, "populate rules failed")
			return
		}

		rm.Set(dbID, r)
	}

	return
}

func getRulesManager(c *gin.Context) (r *resolver.RulesManager) {
	return c.MustGet("rules").(*resolver.RulesManager)
}
