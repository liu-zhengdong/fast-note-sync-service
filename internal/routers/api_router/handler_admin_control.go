package api_router

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/haierkeys/fast-note-sync-service/internal/app"
	"github.com/haierkeys/fast-note-sync-service/internal/config"
	"github.com/haierkeys/fast-note-sync-service/internal/dao"
	"github.com/haierkeys/fast-note-sync-service/internal/dto"
	pkgapp "github.com/haierkeys/fast-note-sync-service/pkg/app"
	"github.com/haierkeys/fast-note-sync-service/pkg/code"
	"github.com/haierkeys/fast-note-sync-service/pkg/fileurl"
	"github.com/haierkeys/fast-note-sync-service/pkg/util"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/process"
	"go.uber.org/zap"
)

// AdminControlHandler Admin control configuration API router handler
// AdminControlHandler 管理控制配置 API 路由处理器
// Uses App Container to inject dependencies
// 使用 App Container 注入依赖
type AdminControlHandler struct {
	*Handler
	wss       *pkgapp.WebsocketServer
	upgradeMu sync.Mutex
}

// NewAdminControlHandler creates AdminControlHandler instance
// NewAdminControlHandler 创建 AdminControlHandler 实例
func NewAdminControlHandler(a *app.App, wss *pkgapp.WebsocketServer) *AdminControlHandler {
	return &AdminControlHandler{
		Handler: NewHandler(a),
		wss:     wss,
	}
}

// Config retrieves WebGUI configuration (public interface)
// @Summary Get WebGUI basic config
// @Description Get non-sensitive configuration required for frontend display, such as font settings, registration status, etc.
// @Tags Config
// @Produce json
// @Success 200 {object} pkgapp.Res{data=dto.AdminWebGUIConfig} "Success"
// @Router /api/webgui/config [get]
func (h *AdminControlHandler) Config(c *gin.Context) {
	response := pkgapp.NewResponse(c)
	cfg := h.App.Config()
	ftsBleveEnabled := true
	if cfg.App.FtsBleveEnabled != nil {
		ftsBleveEnabled = *cfg.App.FtsBleveEnabled
	}
	data := dto.AdminWebGUIConfig{
		FontSet:          cfg.WebGUI.FontSet,
		RegisterIsEnable: h.App.UserService.IsRegisterEnabled(c),
		FtsBleveEnabled:  ftsBleveEnabled,
	}
	response.ToResponse(code.Success.WithData(data))
}

// CheckAdmin checks if current user has admin privileges
// @Summary Check admin permission
// @Description Check if the current logged-in user has system admin privileges
// @Tags Config
// @Security UserAuthToken
// @Produce json
// @Success 200 {object} pkgapp.Res{data=dto.AdminCheckResponse} "Success"
// @Router /api/admin/check [get]
func (h *AdminControlHandler) CheckAdmin(c *gin.Context) {
	response := pkgapp.NewResponse(c)
	cfg := h.App.Config()
	uid := pkgapp.GetUID(c)

	isAdmin := false
	if uid != 0 {
		// AdminUID 0 means no restriction, otherwise must match AdminUID
		// AdminUID 为 0 表示不限制，否则必须匹配 AdminUID
		if cfg.User.AdminUID == 0 || uid == int64(cfg.User.AdminUID) {
			isAdmin = true
		}
	}

	response.ToResponse(code.Success.WithData(dto.AdminCheckResponse{
		IsAdmin: isAdmin,
	}))
}

// GetConfig retrieves admin configuration (requires admin privileges)
// @Summary Get full admin config
// @Description Get full system configuration information, requires admin privileges
// @Tags Config
// @Security UserAuthToken
// @Produce json
// @Success 200 {object} pkgapp.Res{data=dto.AdminConfig} "Success"
// @Failure 403 {object} pkgapp.Res "Insufficient privileges"
// @Router /api/admin/config [get]
func (h *AdminControlHandler) GetConfig(c *gin.Context) {
	response := pkgapp.NewResponse(c)
	cfg := h.App.Config()
	logger := h.App.Logger()

	uid := pkgapp.GetUID(c)
	if uid == 0 {
		logger.Error("apiRouter.WebGUI.GetConfig err uid=0")
		response.ToResponse(code.ErrorInvalidUserAuthToken)
		return
	}

	// Deny access if AdminUID is configured and current user is not an admin
	// 当配置了管理员 UID 且当前用户不是管理员时，拒绝访问
	if cfg.User.AdminUID != 0 && uid != int64(cfg.User.AdminUID) {
		response.ToResponse(code.ErrorUserIsNotAdmin)
		return
	}

	data := &dto.AdminConfig{
		FontSet:                 &cfg.WebGUI.FontSet,
		RegisterIsEnable:        &cfg.User.RegisterIsEnable,
		FileChunkSize:           &cfg.App.FileChunkSize,
		SoftDeleteRetentionTime: &cfg.App.SoftDeleteRetentionTime,
		UploadSessionTimeout:    &cfg.App.UploadSessionTimeout,
		HistoryKeepVersions:     cfg.App.HistoryKeepVersions,
		HistorySaveDelay:        &cfg.App.HistorySaveDelay,
		// DefaultAPIFolder:        &cfg.App.DefaultAPIFolder,
		AdminUID:                      &cfg.User.AdminUID,
		AuthTokenKey:                  &cfg.Security.AuthTokenKey,
		TokenExpiry:                   &cfg.Security.TokenExpiry,
		ShareTokenKey:                 &cfg.Security.ShareTokenKey,
		ShareTokenExpiry:              &cfg.Security.ShareTokenExpiry,
		PullSource:                    &cfg.App.PullSource,
		PullReleaseChannel:            &cfg.App.PullReleaseChannel,
		WebGUILoginTokenExpiry:        &cfg.Security.WebGUILoginTokenExpiry,
		WebGUILoginTokenBindIP:        cfg.Security.WebGUILoginTokenBindIP,
		CustomResponseHeaders:         &cfg.Server.CustomResponseHeaders,
		DefaultPageSize:               &cfg.App.DefaultPageSize,
		MaxPageSize:                   &cfg.App.MaxPageSize,
		DefaultContextTimeout:         &cfg.App.DefaultContextTimeout,
		TempPath:                      &cfg.App.TempPath,
		IsReturnSussess:               &cfg.App.IsReturnSussess,
		SyncLogRetentionTime:          &cfg.App.SyncLogRetentionTime,
		DownloadSessionTimeout:        &cfg.App.DownloadSessionTimeout,
		WorkerPoolMaxWorkers:          &cfg.App.WorkerPoolMaxWorkers,
		WorkerPoolQueueSize:           &cfg.App.WorkerPoolQueueSize,
		WriteQueueCapacity:            &cfg.App.WriteQueueCapacity,
		WriteQueueTimeout:             &cfg.App.WriteQueueTimeout,
		WriteQueueIdleTime:            &cfg.App.WriteQueueIdleTime,
		WebSocketReadMaxPayloadSize:   &cfg.App.WebSocketReadMaxPayloadSize,
		WebSocketWriteMaxPayloadSize:  &cfg.App.WebSocketWriteMaxPayloadSize,
		WebSocketParallelEnabled:      cfg.App.WebSocketParallelEnabled,
		WebSocketParallelGolimit:      &cfg.App.WebSocketParallelGolimit,
		WebSocketCheckUtf8Enabled:     cfg.App.WebSocketCheckUtf8Enabled,
		WebSocketCompressionEnabled:   cfg.App.WebSocketCompressionEnabled,
		WebSocketCompressionLevel:     &cfg.App.WebSocketCompressionLevel,
		WebSocketCompressionThreshold: &cfg.App.WebSocketCompressionThreshold,
		FtsBleveEnabled:               cfg.App.FtsBleveEnabled,
		FtsBleveStoreRaw:              cfg.App.FtsBleveStoreRaw,
		GitName:                       &cfg.Git.Name,
		GitEmail:                      &cfg.Git.Email,
		PipelineWindowUp:              cfg.App.PipelineWindowUp,
		PipelineWindowDown:            cfg.App.PipelineWindowDown,
	}

	response.ToResponse(code.Success.WithData(data))
}

// UpdateConfig updates admin configuration (requires admin privileges)
// @Summary Update admin config
// @Description Modify full system configuration information, requires admin privileges
// @Tags Config
// @Security UserAuthToken
// @Accept json
// @Produce json
// @Param params body dto.AdminConfig true "Config Parameters"
// @Success 200 {object} pkgapp.Res{data=dto.AdminConfig} "Success"
// @Failure 403 {object} pkgapp.Res "Insufficient privileges"
// @Router /api/admin/config [post]
func (h *AdminControlHandler) UpdateConfig(c *gin.Context) {
	params := &dto.AdminConfig{}
	response := pkgapp.NewResponse(c)
	cfg := h.App.Config()
	logger := h.App.Logger()

	valid, errs := pkgapp.BindAndValid(c, params)
	if !valid {
		logger.Error("apiRouter.WebGUI.UpdateConfig.BindAndValid err", zap.Error(errs))
		response.ToResponse(code.ErrorInvalidParams.WithDetails(errs.ErrorsToString()).WithData(errs.MapsToString()))
		return
	}

	uid := pkgapp.GetUID(c)
	if uid == 0 {
		logger.Error("apiRouter.WebGUI.UpdateConfig err uid=0")
		response.ToResponse(code.ErrorInvalidUserAuthToken)
		return
	}

	// Deny access if AdminUID is configured and current user is not an admin
	// 当配置了管理员 UID 且当前用户不是管理员时，拒绝访问
	if cfg.User.AdminUID != 0 && uid != int64(cfg.User.AdminUID) {
		response.ToResponse(code.ErrorUserIsNotAdmin)
		return
	}

	// Validate historyKeepVersions cannot be less than 100
	// 验证 historyKeepVersions 不能小于 100
	if params.HistoryKeepVersions != nil && *params.HistoryKeepVersions > 0 && *params.HistoryKeepVersions < 100 {
		logger.Warn("apiRouter.WebGUI.UpdateConfig invalid historyKeepVersions",
			zap.Int("value", *params.HistoryKeepVersions))
		response.ToResponse(code.ErrorInvalidParams.WithDetails("historyKeepVersions must be at least 100"))
		return
	}

	// Validate historySaveDelay cannot be less than 1 second
	// 验证 historySaveDelay 不能小于 1 秒
	if params.HistorySaveDelay != nil && *params.HistorySaveDelay != "" {
		delay, err := util.ParseDuration(*params.HistorySaveDelay)
		if err != nil {
			logger.Warn("apiRouter.WebGUI.UpdateConfig invalid historySaveDelay format",
				zap.String("value", *params.HistorySaveDelay))
			response.ToResponse(code.ErrorInvalidParams.WithDetails("historySaveDelay format invalid, e.g. 10s, 1m"))
			return
		}
		if delay < 1*time.Second {
			logger.Warn("apiRouter.WebGUI.UpdateConfig historySaveDelay too small",
				zap.String("value", *params.HistorySaveDelay))
			response.ToResponse(code.ErrorInvalidParams.WithDetails("historySaveDelay must be at least 1s"))
			return
		}
	}

	// Validate webguiLoginTokenExpiry format
	// 验证 webguiLoginTokenExpiry 格式
	if params.WebGUILoginTokenExpiry != nil && *params.WebGUILoginTokenExpiry != "" {
		_, err := util.ParseDuration(*params.WebGUILoginTokenExpiry)
		if err != nil {
			logger.Warn("apiRouter.WebGUI.UpdateConfig invalid webguiLoginTokenExpiry format",
				zap.String("value", *params.WebGUILoginTokenExpiry))
			response.ToResponse(code.ErrorInvalidParams.WithDetails("webguiLoginTokenExpiry format invalid, e.g. 7d, 24h, 30m"))
			return
		}
	}

	if params.SyncLogRetentionTime != nil && *params.SyncLogRetentionTime != "" {
		_, err := util.ParseDuration(*params.SyncLogRetentionTime)
		if err != nil {
			logger.Warn("apiRouter.WebGUI.UpdateConfig invalid syncLogRetentionTime format",
				zap.String("value", *params.SyncLogRetentionTime))
			response.ToResponse(code.ErrorInvalidParams.WithDetails("syncLogRetentionTime format invalid, e.g. 30d, 7d"))
			return
		}
	}

	if params.DownloadSessionTimeout != nil && *params.DownloadSessionTimeout != "" {
		_, err := util.ParseDuration(*params.DownloadSessionTimeout)
		if err != nil {
			logger.Warn("apiRouter.WebGUI.UpdateConfig invalid downloadSessionTimeout format",
				zap.String("value", *params.DownloadSessionTimeout))
			response.ToResponse(code.ErrorInvalidParams.WithDetails("downloadSessionTimeout format invalid, e.g. 1h, 30m"))
			return
		}
	}

	if params.WriteQueueTimeout != nil && *params.WriteQueueTimeout != "" {
		_, err := util.ParseDuration(*params.WriteQueueTimeout)
		if err != nil {
			logger.Warn("apiRouter.WebGUI.UpdateConfig invalid writeQueueTimeout format",
				zap.String("value", *params.WriteQueueTimeout))
			response.ToResponse(code.ErrorInvalidParams.WithDetails("writeQueueTimeout format invalid, e.g. 30s, 1m"))
			return
		}
	}

	if params.WriteQueueIdleTime != nil && *params.WriteQueueIdleTime != "" {
		_, err := util.ParseDuration(*params.WriteQueueIdleTime)
		if err != nil {
			logger.Warn("apiRouter.WebGUI.UpdateConfig invalid writeQueueIdleTime format",
				zap.String("value", *params.WriteQueueIdleTime))
			response.ToResponse(code.ErrorInvalidParams.WithDetails("writeQueueIdleTime format invalid, e.g. 10m, 1h"))
			return
		}
	}

	// Update configuration
	// 更新配置
	if params.FontSet != nil {
		cfg.WebGUI.FontSet = *params.FontSet
	}
	if params.RegisterIsEnable != nil {
		cfg.User.RegisterIsEnable = *params.RegisterIsEnable
	}
	if params.FileChunkSize != nil {
		cfg.App.FileChunkSize = *params.FileChunkSize
	}
	if params.SoftDeleteRetentionTime != nil {
		cfg.App.SoftDeleteRetentionTime = *params.SoftDeleteRetentionTime
	}
	if params.UploadSessionTimeout != nil {
		cfg.App.UploadSessionTimeout = *params.UploadSessionTimeout
	}
	if params.HistoryKeepVersions != nil {
		cfg.App.HistoryKeepVersions = params.HistoryKeepVersions
	}
	if params.HistorySaveDelay != nil {
		cfg.App.HistorySaveDelay = *params.HistorySaveDelay
	}
	if params.AdminUID != nil {
		cfg.User.AdminUID = *params.AdminUID
	}
	if params.AuthTokenKey != nil {
		cfg.Security.AuthTokenKey = *params.AuthTokenKey
	}
	if params.TokenExpiry != nil {
		cfg.Security.TokenExpiry = *params.TokenExpiry
	}
	if params.ShareTokenKey != nil {
		cfg.Security.ShareTokenKey = *params.ShareTokenKey
	}
	if params.ShareTokenExpiry != nil {
		cfg.Security.ShareTokenExpiry = *params.ShareTokenExpiry
	}
	if params.PullSource != nil {
		cfg.App.PullSource = *params.PullSource
		// Sync the running SourceSelector so the change takes effect immediately
		// instead of waiting for a server restart.
		// 同步到运行中的 SourceSelector，使改动立即生效，无需重启服务端。
		h.App.SetPullSourceMode(*params.PullSource)
	}
	if params.PullReleaseChannel != nil {
		cfg.App.PullReleaseChannel = *params.PullReleaseChannel
	}
	if params.WebGUILoginTokenExpiry != nil {
		cfg.Security.WebGUILoginTokenExpiry = *params.WebGUILoginTokenExpiry
	}
	if params.WebGUILoginTokenBindIP != nil {
		cfg.Security.WebGUILoginTokenBindIP = params.WebGUILoginTokenBindIP
	}
	if params.CustomResponseHeaders != nil {
		cfg.Server.CustomResponseHeaders = *params.CustomResponseHeaders
	}
	if params.DefaultPageSize != nil {
		cfg.App.DefaultPageSize = *params.DefaultPageSize
	}
	if params.MaxPageSize != nil {
		cfg.App.MaxPageSize = *params.MaxPageSize
	}
	if params.DefaultContextTimeout != nil {
		cfg.App.DefaultContextTimeout = *params.DefaultContextTimeout
	}
	if params.TempPath != nil {
		cfg.App.TempPath = *params.TempPath
	}
	if params.IsReturnSussess != nil {
		cfg.App.IsReturnSussess = *params.IsReturnSussess
	}
	if params.SyncLogRetentionTime != nil {
		cfg.App.SyncLogRetentionTime = *params.SyncLogRetentionTime
	}
	if params.DownloadSessionTimeout != nil {
		cfg.App.DownloadSessionTimeout = *params.DownloadSessionTimeout
	}
	if params.WorkerPoolMaxWorkers != nil {
		cfg.App.WorkerPoolMaxWorkers = *params.WorkerPoolMaxWorkers
	}
	if params.WorkerPoolQueueSize != nil {
		cfg.App.WorkerPoolQueueSize = *params.WorkerPoolQueueSize
	}
	if params.WriteQueueCapacity != nil {
		cfg.App.WriteQueueCapacity = *params.WriteQueueCapacity
	}
	if params.WriteQueueTimeout != nil {
		cfg.App.WriteQueueTimeout = *params.WriteQueueTimeout
	}
	if params.WriteQueueIdleTime != nil {
		cfg.App.WriteQueueIdleTime = *params.WriteQueueIdleTime
	}
	if params.WebSocketReadMaxPayloadSize != nil {
		cfg.App.WebSocketReadMaxPayloadSize = *params.WebSocketReadMaxPayloadSize
	}
	if params.WebSocketWriteMaxPayloadSize != nil {
		cfg.App.WebSocketWriteMaxPayloadSize = *params.WebSocketWriteMaxPayloadSize
	}
	if params.WebSocketParallelEnabled != nil {
		cfg.App.WebSocketParallelEnabled = params.WebSocketParallelEnabled
	}
	if params.WebSocketParallelGolimit != nil {
		cfg.App.WebSocketParallelGolimit = *params.WebSocketParallelGolimit
	}
	if params.WebSocketCheckUtf8Enabled != nil {
		cfg.App.WebSocketCheckUtf8Enabled = params.WebSocketCheckUtf8Enabled
	}
	if params.WebSocketCompressionEnabled != nil {
		cfg.App.WebSocketCompressionEnabled = params.WebSocketCompressionEnabled
	}
	if params.WebSocketCompressionLevel != nil {
		cfg.App.WebSocketCompressionLevel = *params.WebSocketCompressionLevel
	}
	if params.WebSocketCompressionThreshold != nil {
		cfg.App.WebSocketCompressionThreshold = *params.WebSocketCompressionThreshold
	}
	if params.FtsBleveEnabled != nil {
		cfg.App.FtsBleveEnabled = params.FtsBleveEnabled
	}
	if params.FtsBleveStoreRaw != nil {
		cfg.App.FtsBleveStoreRaw = params.FtsBleveStoreRaw
	}
	if params.GitName != nil {
		cfg.Git.Name = *params.GitName
	}
	if params.GitEmail != nil {
		cfg.Git.Email = *params.GitEmail
	}
	if params.PipelineWindowUp != nil {
		// Stored as-is (may exceed the read-time clamp); PipelineWindowUpClamped()
		// clamps to [0,32] wherever it's consumed (auth response / ClientInfo).
		// 原样存储（可能超出读取时的钳制范围）；PipelineWindowUpClamped() 在消费处
		// （auth 响应 / ClientInfo）统一钳制到 [0,32]。
		cfg.App.PipelineWindowUp = params.PipelineWindowUp
	}
	if params.PipelineWindowDown != nil {
		cfg.App.PipelineWindowDown = params.PipelineWindowDown
	}

	// Save configuration to file
	// 保存配置到文件
	if err := cfg.Save(); err != nil {
		logger.Error("apiRouter.WebGUI.UpdateConfig.Save err", zap.Error(err))
		response.ToResponse(code.ErrorConfigSaveFailed)
		return
	}

	response.ToResponse(code.Success.WithData(params))
}

// GetUserDatabaseConfig retrieves user database configuration (requires admin privileges)
// @Summary Get user database config
// @Description Get user database configuration information, requires admin privileges
// @Tags Config
// @Security UserAuthToken
// @Produce json
// @Success 200 {object} pkgapp.Res{data=dto.AdminUserDatabaseConfig} "Success"
// @Failure 403 {object} pkgapp.Res "Insufficient privileges"
// @Router /api/admin/config/user_database [get]
func (h *AdminControlHandler) GetUserDatabaseConfig(c *gin.Context) {
	response := pkgapp.NewResponse(c)
	cfg := h.App.Config()
	logger := h.App.Logger()

	uid := pkgapp.GetUID(c)
	if uid == 0 {
		logger.Error("apiRouter.AdminControl.GetUserDatabaseConfig err uid=0")
		response.ToResponse(code.ErrorInvalidUserAuthToken)
		return
	}

	// Deny access if AdminUID is configured and current user is not an admin
	// 当配置了管理员 UID 且当前用户不是管理员时，拒绝访问
	if cfg.User.AdminUID != 0 && uid != int64(cfg.User.AdminUID) {
		response.ToResponse(code.ErrorUserIsNotAdmin)
		return
	}

	dbCfg := cfg.UserDatabase
	maxIdleConns := 0
	if dbCfg.MaxIdleConns != nil {
		maxIdleConns = *dbCfg.MaxIdleConns
	}
	maxOpenConns := 0
	if dbCfg.MaxOpenConns != nil {
		maxOpenConns = *dbCfg.MaxOpenConns
	}
	data := &dto.AdminUserDatabaseConfig{
		Type:                dbCfg.Type,
		Path:                dbCfg.Path,
		UserName:            dbCfg.UserName,
		Password:            dbCfg.Password,
		Host:                dbCfg.Host,
		Port:                dbCfg.Port,
		Name:                dbCfg.Name,
		SSLMode:             dbCfg.SSLMode,
		Schema:              dbCfg.Schema,
		MaxIdleConns:        maxIdleConns,
		MaxOpenConns:        maxOpenConns,
		ConnMaxLifetime:     dbCfg.ConnMaxLifetime,
		ConnMaxIdleTime:     dbCfg.ConnMaxIdleTime,
		MaxWriteConcurrency: dbCfg.MaxWriteConcurrency,
		Charset:             dbCfg.Charset,
		ParseTime:           dbCfg.ParseTime,
	}
	response.ToResponse(code.Success.WithData(data))
}

// UpdateUserDatabaseConfig updates user database configuration (requires admin privileges)
// @Summary Update user database config
// @Description Modify user database configuration information, requires admin privileges
// @Tags Config
// @Security UserAuthToken
// @Accept json
// @Produce json
// @Param params body dto.AdminUserDatabaseConfig true "Config Parameters"
// @Success 200 {object} pkgapp.Res{data=dto.AdminUserDatabaseConfig} "Success"
// @Failure 403 {object} pkgapp.Res "Insufficient privileges"
// @Router /api/admin/config/user_database [post]
func (h *AdminControlHandler) UpdateUserDatabaseConfig(c *gin.Context) {
	params := &dto.AdminUserDatabaseConfig{}
	response := pkgapp.NewResponse(c)
	cfg := h.App.Config()
	logger := h.App.Logger()

	valid, errs := pkgapp.BindAndValid(c, params)
	if !valid {
		logger.Error("apiRouter.AdminControl.UpdateUserDatabaseConfig.BindAndValid err", zap.Error(errs))
		response.ToResponse(code.ErrorInvalidParams.WithDetails(errs.ErrorsToString()).WithData(errs.MapsToString()))
		return
	}

	uid := pkgapp.GetUID(c)
	if uid == 0 {
		logger.Error("apiRouter.AdminControl.UpdateUserDatabaseConfig err uid=0")
		response.ToResponse(code.ErrorInvalidUserAuthToken)
		return
	}

	// Deny access if AdminUID is configured and current user is not an admin
	// 当配置了管理员 UID 且当前用户不是管理员时，拒绝访问
	if cfg.User.AdminUID != 0 && uid != int64(cfg.User.AdminUID) {
		response.ToResponse(code.ErrorUserIsNotAdmin)
		return
	}

	// Update configuration
	// 更新配置
	cfg.UserDatabase.Type = params.Type
	cfg.UserDatabase.Path = params.Path
	cfg.UserDatabase.UserName = params.UserName
	cfg.UserDatabase.Password = params.Password
	cfg.UserDatabase.Host = params.Host
	cfg.UserDatabase.Port = params.Port
	cfg.UserDatabase.Name = params.Name
	cfg.UserDatabase.SSLMode = params.SSLMode
	cfg.UserDatabase.Schema = params.Schema
	cfg.UserDatabase.MaxIdleConns = &params.MaxIdleConns
	cfg.UserDatabase.MaxOpenConns = &params.MaxOpenConns
	cfg.UserDatabase.ConnMaxLifetime = params.ConnMaxLifetime
	cfg.UserDatabase.ConnMaxIdleTime = params.ConnMaxIdleTime
	cfg.UserDatabase.MaxWriteConcurrency = params.MaxWriteConcurrency
	cfg.UserDatabase.Charset = params.Charset
	cfg.UserDatabase.ParseTime = params.ParseTime

	// MySQL specific hardcoded defaults
	// MySQL 的硬编码默认逻辑
	if params.Type == "mysql" {
		cfg.UserDatabase.Charset = "utf8mb4"
		cfg.UserDatabase.ParseTime = true
	}

	if params.Type == "sqlite" {
		enableQueue := true
		cfg.UserDatabase.EnableWriteQueue = &enableQueue
	} else if params.Type == "mysql" || params.Type == "postgres" {
		enableQueue := false
		cfg.UserDatabase.EnableWriteQueue = &enableQueue
	}

	// Save configuration to file
	// 保存配置到文件
	if err := cfg.Save(); err != nil {
		logger.Error("apiRouter.AdminControl.UpdateUserDatabaseConfig.Save err", zap.Error(err))
		response.ToResponse(code.ErrorConfigSaveFailed)
		return
	}

	response.ToResponse(code.Success.WithData(params))
}

// ValidateUserDatabaseConfig tests user database connection (requires admin privileges)
// @Summary Test user database connection
// ValidateUserDatabaseConfig tests user database connection (requires admin privileges)
// @Summary Test user database connection
// @Description Test if the provided database configuration can connect successfully, requires admin privileges
// @Tags Config
// @Security UserAuthToken
// @Accept json
// @Produce json
// @Param params body dto.AdminUserDatabaseConfig true "Config Parameters"
// @Success 200 {object} pkgapp.Res "Success"
// @Failure 400 {object} pkgapp.Res "Connection failed"
// @Router /api/admin/config/user_database/test [post]
func (h *AdminControlHandler) ValidateUserDatabaseConfig(c *gin.Context) {
	params := &dto.AdminUserDatabaseConfig{}
	response := pkgapp.NewResponse(c)
	logger := h.App.Logger()

	valid, errs := pkgapp.BindAndValid(c, params)
	if !valid {
		logger.Error("apiRouter.AdminControl.ValidateUserDatabaseConfig.BindAndValid err", zap.Error(errs))
		response.ToResponse(code.ErrorInvalidParams.WithDetails(errs.ErrorsToString()).WithData(errs.MapsToString()))
		return
	}

	uid := pkgapp.GetUID(c)
	if uid == 0 {
		response.ToResponse(code.ErrorInvalidUserAuthToken)
		return
	}

	cfg := h.App.Config()
	// Deny access if AdminUID is configured and current user is not an admin
	// 当配置了管理员 UID 且当前用户不是管理员时，拒绝访问
	if cfg.User.AdminUID != 0 && uid != int64(cfg.User.AdminUID) {
		response.ToResponse(code.ErrorUserIsNotAdmin)
		return
	}

	// Map DTO to DatabaseConfig
	// 将 DTO 映射到 DatabaseConfig
	enableQueue := false
	if params.Type == "sqlite" {
		enableQueue = true
	}
	autoMigrate := true
	dbCfg := config.DatabaseConfig{
		Type:                params.Type,
		Path:                params.Path,
		UserName:            params.UserName,
		Password:            params.Password,
		Host:                params.Host,
		Port:                params.Port,
		Name:                params.Name,
		SSLMode:             params.SSLMode,
		Schema:              params.Schema,
		AutoMigrate:         &autoMigrate,
		MaxIdleConns:        &params.MaxIdleConns,
		MaxOpenConns:        &params.MaxOpenConns,
		ConnMaxLifetime:     params.ConnMaxLifetime,
		ConnMaxIdleTime:     params.ConnMaxIdleTime,
		EnableWriteQueue:    &enableQueue,
		MaxWriteConcurrency: params.MaxWriteConcurrency,
		Charset:             params.Charset,
		ParseTime:           params.ParseTime,
	}

	// Apply hardcoded default rules for MySQL during validation
	// 在测试连接时也应用 MySQL 的硬编码默认规则
	if params.Type == "mysql" {
		dbCfg.Charset = "utf8mb4"
		dbCfg.ParseTime = true
	}

	// Use dao.NewEngine to test connection
	// 使用 dao.NewEngine 测试连接
	db, err := dao.NewEngine(dbCfg, h.App.Logger())
	if err != nil {
		logger.Warn("Database connection test failed", zap.Error(err))
		response.ToResponse(code.Failed.WithDetails("Connection failed: " + err.Error()))
		return
	}

	sqlDB, err := db.DB()
	if err != nil {
		response.ToResponse(code.Failed.WithDetails("Failed to get DB instance: " + err.Error()))
		return
	}
	defer sqlDB.Close()

	if err := sqlDB.Ping(); err != nil {
		logger.Warn("Database ping failed", zap.Error(err))
		response.ToResponse(code.Failed.WithDetails("Ping failed: " + err.Error()))
		return
	}

	// For MySQL, verify CREATE DATABASE privilege
	// 针对 MySQL，验证是否具有创建数据库的权限
	if params.Type == "mysql" {
		tempDBName := fmt.Sprintf("fn_perm_check_%d", time.Now().Unix())
		if err := db.Exec(fmt.Sprintf("CREATE DATABASE %s", tempDBName)).Error; err != nil {
			logger.Warn("MySQL CREATE DATABASE permission test failed", zap.Error(err))
			response.ToResponse(code.Failed.WithDetails("Missing CREATE DATABASE permission: " + err.Error()))
			return
		}
		// Clean up immediately after successful verification
		// 验证成功后立即清理
		_ = db.Exec(fmt.Sprintf("DROP DATABASE %s", tempDBName))
	}

	response.ToResponse(code.Success.WithDetails("Database connection and permission verification successful"))
}

// GetCloudflareConfig retrieves Cloudflare tunnel configuration (requires admin privileges)
// @Summary Get Cloudflare config
// @Description Get Cloudflare tunnel configuration, requires admin privileges
// @Tags Config
// @Security UserAuthToken
// @Produce json
// @Success 200 {object} pkgapp.Res{data=dto.AdminCloudflareConfig} "Success"
// @Failure 403 {object} pkgapp.Res "Insufficient privileges"
// @Router /api/admin/config/cloudflare [get]
func (h *AdminControlHandler) GetCloudflareConfig(c *gin.Context) {
	response := pkgapp.NewResponse(c)
	cfg := h.App.Config()
	logger := h.App.Logger()

	uid := pkgapp.GetUID(c)
	if uid == 0 {
		logger.Error("apiRouter.AdminControl.GetCloudflareConfig err uid=0")
		response.ToResponse(code.ErrorInvalidUserAuthToken)
		return
	}

	if cfg.User.AdminUID != 0 && uid != int64(cfg.User.AdminUID) {
		response.ToResponse(code.ErrorUserIsNotAdmin)
		return
	}

	data := &dto.AdminCloudflareConfig{
		Enabled:    cfg.Cloudflare.Enabled,
		Token:      cfg.Cloudflare.Token,
		LogEnabled: cfg.Cloudflare.LogEnabled,
	}

	response.ToResponse(code.Success.WithData(data))
}

// UpdateCloudflareConfig updates Cloudflare tunnel configuration (requires admin privileges)
// @Summary Update Cloudflare config
// @Description Modify Cloudflare tunnel configuration, requires admin privileges
// @Tags Config
// @Security UserAuthToken
// @Accept json
// @Produce json
// @Param params body dto.AdminCloudflareConfig true "Config Parameters"
// @Success 200 {object} pkgapp.Res{data=dto.AdminCloudflareConfig} "Success"
// @Failure 403 {object} pkgapp.Res "Insufficient privileges"
// @Router /api/admin/config/cloudflare [post]
func (h *AdminControlHandler) UpdateCloudflareConfig(c *gin.Context) {
	params := &dto.AdminCloudflareConfig{}
	response := pkgapp.NewResponse(c)
	cfg := h.App.Config()
	logger := h.App.Logger()

	valid, errs := pkgapp.BindAndValid(c, params)
	if !valid {
		logger.Error("apiRouter.AdminControl.UpdateCloudflareConfig.BindAndValid err", zap.Error(errs))
		response.ToResponse(code.ErrorInvalidParams.WithDetails(errs.ErrorsToString()).WithData(errs.MapsToString()))
		return
	}

	uid := pkgapp.GetUID(c)
	if uid == 0 {
		logger.Error("apiRouter.AdminControl.UpdateCloudflareConfig err uid=0")
		response.ToResponse(code.ErrorInvalidUserAuthToken)
		return
	}

	if cfg.User.AdminUID != 0 && uid != int64(cfg.User.AdminUID) {
		response.ToResponse(code.ErrorUserIsNotAdmin)
		return
	}

	if params.Enabled && !h.App.CloudflareService.IsBinaryExist() {
		logger.Error("apiRouter.AdminControl.UpdateCloudflareConfig err: cloudflared binary not found")
		response.ToResponse(code.ErrorCloudflaredBinaryNotFound)
		return
	}

	cfg.Cloudflare.Enabled = params.Enabled
	cfg.Cloudflare.Token = params.Token
	cfg.Cloudflare.LogEnabled = params.LogEnabled

	if err := cfg.Save(); err != nil {
		logger.Error("apiRouter.AdminControl.UpdateCloudflareConfig.Save err", zap.Error(err))
		response.ToResponse(code.ErrorConfigSaveFailed)
		return
	}

	response.ToResponse(code.Success.WithData(params))
}

// CreateUser create a new user (requires admin privileges)
// @Summary Create a new user
// @Description Create a new user, requires admin privileges
// @Tags Config
// @Security UserAuthToken
// @Accept json
// @Produce json
// @Param params body dto.UserCreateRequest true "Config Parameters"
// @Success 200 {object} pkgapp.Res{data=dto.UserDTO} "Success"
// @Failure 403 {object} pkgapp.Res "Insufficient privileges"
// @Router /api/admin/users/create [post]
func (h *AdminControlHandler) CreateUser(c *gin.Context) {
	cfg := h.App.Config()
	logger := h.App.Logger()
	response := pkgapp.NewResponse(c)
	params := &dto.UserCreateRequest{}

	uid := pkgapp.GetUID(c)
	if uid == 0 {
		logger.Error("apiRouter.AdminControl.CreateUser err uid=0")
		response.ToResponse(code.ErrorInvalidUserAuthToken)
		return
	}

	if cfg.User.AdminUID != 0 && uid != int64(cfg.User.AdminUID) {
		response.ToResponse(code.ErrorUserIsNotAdmin)
		return
	}

	// Parameter binding and validation
	valid, errs := pkgapp.BindAndValid(c, params)
	if !valid {
		h.App.Logger().Error("apiRouter.AdminControl.CreateUser.BindAndValid errs", zap.Error(errs))
		response.ToResponse(code.ErrorInvalidParams.WithDetails(errs.ErrorsToString()).WithData(errs.MapsToString()))
		return
	}

	ctx := c.Request.Context()

	// Call UserService to perform create user
	userDTO, err := h.App.UserService.Create(ctx, params)
	if err != nil {
		logger.Error("apiRouter.AdminControl.CreateUser.Create err", zap.Error(err))
		var codeErr *code.Code
		if errors.As(err, &codeErr) {
			response.ToResponse(codeErr)
		} else {
			response.ToResponse(code.ErrorUserRegister.WithDetails(err.Error()))
		}
		return
	}

	response.ToResponse(code.Success.WithData(userDTO))
}

// UpdateUser update a user (requires admin privileges)
// @Summary Update a user
// @Description Update a user, requires admin privileges
// @Tags Config
// @Security UserAuthToken
// @Accept json
// @Produce json
// @Param params body dto.UserUpdateRequest true "Config Parameters"
// @Success 200 {object} pkgapp.Res{data=dto.UserDTO} "Success"
// @Failure 403 {object} pkgapp.Res "Insufficient privileges"
// @Router /api/admin/users/update [post]
func (h *AdminControlHandler) UpdateUser(c *gin.Context) {
	cfg := h.App.Config()
	logger := h.App.Logger()
	response := pkgapp.NewResponse(c)
	params := &dto.UserUpdateRequest{}

	uid := pkgapp.GetUID(c)
	if uid == 0 {
		logger.Error("apiRouter.AdminControl.UpdateUser err uid=0")
		response.ToResponse(code.ErrorInvalidUserAuthToken)
		return
	}

	if cfg.User.AdminUID != 0 && uid != int64(cfg.User.AdminUID) {
		response.ToResponse(code.ErrorUserIsNotAdmin)
		return
	}

	// Parameter binding and validation
	valid, errs := pkgapp.BindAndValid(c, params)

	if !valid {
		h.App.Logger().Error("apiRouter.AdminControl.UpdateUser.BindAndValid errs", zap.Error(errs))
		response.ToResponse(code.ErrorInvalidParams.WithDetails(errs.ErrorsToString()).WithData(errs.MapsToString()))
		return
	}

	// Prevent block an administrator
	if params.IsDeleted {
		if params.UID == int64(cfg.User.AdminUID) {
			response.ToResponse(code.ErrorUserAdminBlock.WithDetails(errs.ErrorsToString()).WithData(errs.MapsToString()))
			return
		}
	}

	ctx := c.Request.Context()

	// Call UserService to perform update user
	err := h.App.UserService.Update(ctx, params)
	if err != nil {
		logger.Error("apiRouter.AdminControl.UpdateUser.Update err", zap.Error(err))
		var codeErr *code.Code
		if errors.As(err, &codeErr) {
			response.ToResponse(codeErr)
		} else {
			response.ToResponse(code.ErrorUserUpdate.WithDetails(err.Error()))
		}
		return
	}

	response.ToResponse(code.SuccessUpdate)
}

// GetUsers retrieves all users info
// @Summary Get all users
// @Description Handle request to get all users.
// @Tags Config
// @Security UserAuthToken
// @Produce json
// @Param pagination query pkgapp.PaginationRequest true "Pagination Parameters"
// @Success 200 {object} pkgapp.Res{data=pkgapp.ListRes{list=[]dto.UserDTO}} "Success"
// @Failure 401 {object} pkgapp.Res "Unauthorized"
// @Router /api/admin/users/list [get]
func (h *AdminControlHandler) GetUsers(c *gin.Context) {
	response := pkgapp.NewResponse(c)
	cfg := h.App.Config()
	logger := h.App.Logger()

	uid := pkgapp.GetUID(c)
	if uid == 0 {
		logger.Error("apiRouter.AdminControl.GetUsers err uid=0")
		response.ToResponse(code.ErrorInvalidUserAuthToken)
		return
	}

	if cfg.User.AdminUID != 0 && uid != int64(cfg.User.AdminUID) {
		response.ToResponse(code.ErrorUserIsNotAdmin)
		return
	}

	// Get request context
	// 获取请求上下文
	ctx := c.Request.Context()
	pager := pkgapp.NewPager(c)

	// Call UserService to get users with pagination // 调用 UserService 分页查询用户列表
	userDTO, total, err := h.App.UserService.GetList(ctx, pager)
	if err != nil {
		logger.Error("apiRouter.AdminControl.GetUsers.GetList err", zap.Error(err))
		response.ToResponse(code.ErrorUserNotFound)
		return
	}

	response.ToResponseList(code.Success, userDTO, int(total))
}

// GetSystemInfo retrieves system monitoring information (requires admin privileges)
// @Summary Get system stats
// @Description Get server runtime, CPU, memory, host and process info, requires admin privileges
// @Tags System
// @Produce json
// @Security UserAuthToken
// @Success 200 {object} pkgapp.Res{data=dto.AdminSystemInfo} "Success"
// @Router /api/admin/system/info [get]
func (h *AdminControlHandler) GetSystemInfo(c *gin.Context) {
	response := pkgapp.NewResponse(c)
	cfg := h.App.Config()
	logger := h.App.Logger()

	uid := pkgapp.GetUID(c)
	if uid == 0 {
		logger.Error("apiRouter.WebGUI.GetSystemInfo err uid=0")
		response.ToResponse(code.ErrorInvalidUserAuthToken)
		return
	}

	if cfg.User.AdminUID != 0 && uid != int64(cfg.User.AdminUID) {
		response.ToResponse(code.ErrorUserIsNotAdmin)
		return
	}

	// Go Runtime
	// Go 运行时状态
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	// CPU
	// CPU 信息
	cpuInfoList, _ := cpu.Info()
	cpuModel := ""
	if len(cpuInfoList) > 0 {
		cpuModel = cpuInfoList[0].ModelName
	}
	physCores, _ := cpu.Counts(false)
	logicCores, _ := cpu.Counts(true)
	cpuPercents, _ := cpu.Percent(time.Second, true)
	loadStat, _ := load.Avg()

	// Memory
	// 内存与交换区信息
	vMem, _ := mem.VirtualMemory()
	swapMem, _ := mem.SwapMemory()

	// Host
	// 主机系统信息
	hInfo, _ := host.Info()

	// Process
	// 当前进程状态
	p, _ := process.NewProcess(int32(os.Getpid()))
	var pName string
	var pPPid int32
	var pCPU float64
	var pMem float32
	if p != nil {
		// Only invoke methods if the process instance is not nil to prevent panic
		// 仅当进程实例不为 nil 时调用方法，以防 panic
		pName, _ = p.Name()
		pPPid, _ = p.Ppid()
		pCPU, _ = p.CPUPercent()
		pMem, _ = p.MemoryPercent()
	}

	data := dto.AdminSystemInfo{
		StartTime: h.App.StartTime,
		Uptime:    time.Since(h.App.StartTime).Seconds(),
		RuntimeStatus: dto.AdminRuntimeInfo{
			NumGoroutine: runtime.NumGoroutine(),
			MemAlloc:     m.Alloc,
			MemTotal:     m.TotalAlloc,
			MemSys:       m.Sys,
			HeapSys:      m.HeapSys,
			HeapIdle:     m.HeapIdle,
			HeapInuse:    m.HeapInuse,
			HeapReleased: m.HeapReleased,
			StackSys:     m.StackSys,
			MSpanSys:     m.MSpanSys,
			MCacheSys:    m.MCacheSys,
			BuckHashSys:  m.BuckHashSys,
			GCSys:        m.GCSys,
			OtherSys:     m.OtherSys,
			NextGC:       m.NextGC,
			NumGC:        m.NumGC,
		},
		CPU: dto.AdminCPUInfo{
			ModelName:     cpuModel,
			PhysicalCores: physCores,
			LogicalCores:  logicCores,
			Percent:       cpuPercents,
			LoadAvg: func() *dto.AdminLoadInfo {
				// Safely check nil loadStat to prevent panic on Windows/unsupported platforms
				// 安全地校验 nil 指针，防止在 Windows 等不支持的平台上产生 panic
				if loadStat == nil {
					return nil
				}
				return &dto.AdminLoadInfo{
					Load1:  loadStat.Load1,
					Load5:  loadStat.Load5,
					Load15: loadStat.Load15,
				}
			}(),
		},
		Memory: func() dto.AdminMemoryInfo {
			var info dto.AdminMemoryInfo
			// Safely handle nil memory structures
			// 安全地处理 nil 的内存结构体
			if vMem != nil {
				info.Total = vMem.Total
				info.Available = vMem.Available
				info.Used = vMem.Used
				info.UsedPercent = vMem.UsedPercent
			}
			if swapMem != nil {
				info.SwapTotal = swapMem.Total
				info.SwapUsed = swapMem.Used
				info.SwapUsedPercent = swapMem.UsedPercent
			}
			return info
		}(),
		Host: func() dto.AdminHostInfo {
			var info dto.AdminHostInfo
			// Safely handle nil host information structure
			// 安全地处理 nil 的主机信息结构体
			if hInfo != nil {
				info.Hostname = hInfo.Hostname
				info.OS = hInfo.OS
				info.Platform = hInfo.Platform
				info.Arch = hInfo.KernelArch
				info.KernelVersion = hInfo.KernelVersion
				info.Uptime = hInfo.Uptime
			}
			info.OSPretty = util.GetOSPrettyName()
			info.CurrentTime = time.Now()
			info.TimeZone = time.Now().Location().String()
			info.TimeZoneOffset = func() int {
				_, offset := time.Now().Zone()
				return offset
			}()
			return info
		}(),
		Process: func() dto.AdminProcessInfo {
			var info dto.AdminProcessInfo
			// Safely handle nil process pointer
			// 安全地处理 nil 的进程指针
			if p != nil {
				info.PID = p.Pid
			}
			info.PPID = pPPid
			info.Name = pName
			info.CPUPercent = pCPU
			info.MemoryPercent = pMem
			return info
		}(),
	}

	response.ToResponse(code.Success.WithData(data))
}

// Upgrade triggers server automatic upgrade.
// The download is not limited by default-context-timeout. pull-source=auto tries
// GitHub and the configured accelerators, then refuses the archive unless it
// matches the release SHA256SUMS. pull-source=github stays on GitHub directly.
// @Summary Trigger server upgrade
// @Description Download the selected fork release, verify SHA256SUMS, and restart. Not limited by the default 60s request timeout.
// @Tags System
// @Produce json
// @Security UserAuthToken
// @Param version query string true "Version to upgrade (e.g. 2.0.10 or latest)"
// @Success 200 {object} pkgapp.Res "Success"
// @Router /api/admin/upgrade [get]
func (h *AdminControlHandler) Upgrade(c *gin.Context) {
	response := pkgapp.NewResponse(c)
	cfg := h.App.Config()
	uid := pkgapp.GetUID(c)

	if cfg.User.AdminUID != 0 && uid != int64(cfg.User.AdminUID) {
		response.ToResponse(code.ErrorUserIsNotAdmin)
		return
	}

	var upgradeReq dto.UpgradeRequest
	if ok, validErrs := pkgapp.BindAndValid(c, &upgradeReq); !ok {
		response.ToResponse(code.ErrorInvalidParams.WithDetails(validErrs.Errors()...))
		return
	}

	// Validate both explicit requests and the cached latest release tag.
	checkInfo := h.App.CheckVersion("")
	version := upgradeReq.Version
	if version == "latest" {
		if !checkInfo.VersionIsNew {
			response.ToResponse(code.Success.WithDetails("Current version is already up to date"))
			return
		}
		version = checkInfo.VersionNewTag
		if version == "" {
			version = checkInfo.VersionNewName
		}
	} else {
		// Clients display versions without v; resolve every cached release's exact tag.
		version = h.App.ServiceReleaseTag(version)
	}
	goos, goarch := runtime.GOOS, runtime.GOARCH
	downloadURL, fileName, err := fileurl.ServiceDownload(version, goos, goarch)
	if err != nil {
		response.ToResponse(code.ErrorInvalidParams.WithDetails(err.Error()))
		return
	}
	sumsURL, err := fileurl.ServiceChecksumURL(version)
	if err != nil {
		response.ToResponse(code.ErrorInvalidParams.WithDetails(err.Error()))
		return
	}
	versionRaw := strings.TrimPrefix(version, "v")

	if !h.upgradeMu.TryLock() {
		response.ToResponse(code.ErrorServerBusy.WithDetails("An upgrade is already in progress"))
		return
	}
	defer h.upgradeMu.Unlock()

	// The API middleware cancels the request context after default-context-timeout
	// (60s) and the server write timeout is also 60s. Neither may abort this download.
	// 接口中间件会在 default-context-timeout（60 秒）取消请求上下文，服务写超时同样是 60 秒。
	// 下载不能被这两处掐断。
	dlCtx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), fileurl.UpgradeBudget)
	defer cancel()
	extendWriteDeadline(c.Writer, fileurl.UpgradeBudget)

	h.App.Logger().Info("Starting upgrade download",
		zap.String("url", downloadURL),
		zap.String("version", versionRaw),
		zap.String("pullSource", cfg.App.PullSource),
	)

	// Prepare temp directory
	// 使用配置中的 TempPath 作为临时目录
	tempDir := filepath.Join(cfg.App.TempPath, "upgrade")
	_ = os.RemoveAll(tempDir)
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		response.ToResponse(code.Failed.WithDetails("Failed to create temp directory: " + err.Error()))
		return
	}

	tarPath := filepath.Join(tempDir, fileName)
	downloaded, err := fileurl.DownloadVerified(dlCtx, downloadURL, fileName, sumsURL, fileurl.ServiceFetchConfig{
		Mode:    cfg.App.PullSource,
		Proxies: cfg.App.GitHubProxies,
		Hooks: fileurl.FetchHooks{
			Warn: func(msg, rawURL string, err error) {
				h.App.Logger().Warn(msg, zap.String("url", rawURL), zap.Error(err))
			},
			Info: func(msg, rawURL string) {
				h.App.Logger().Info(msg, zap.String("url", rawURL))
			},
		},
	}, tarPath)
	if err != nil {
		h.App.Logger().Error("Upgrade download failed",
			zap.String("url", downloadURL),
			zap.Error(err),
		)
		response.ToResponse(code.Failed.WithDetails("Download failed: " + err.Error()))
		return
	}
	h.App.Logger().Info("Upgrade archive verified",
		zap.String("url", downloaded.URL),
		zap.String("sha256", downloaded.SHA256),
		zap.Bool("checksumFromGitHub", downloaded.ChecksumDirect),
	)

	// Extract
	binaryName := "fast-note-sync-service"
	if goos == "windows" {
		binaryName += ".exe"
	}
	extractedBinaryPath := filepath.Join(tempDir, binaryName)

	if err := h.extractBinary(tarPath, tempDir, binaryName); err != nil {
		h.App.Logger().Error("Upgrade extract failed", zap.Error(err))
		response.ToResponse(code.Failed.WithDetails("Extract failed: " + err.Error()))
		return
	}

	// Trigger upgrade in App
	h.App.TriggerUpgrade(extractedBinaryPath)

	response.ToResponse(code.Success.WithDetails("Upgrade triggered, server is restarting..."))
}

// Restart triggers server automatic restart
// @Summary Trigger server restart
// @Description Gracefully restart the server
// @Tags System
// @Produce json
// @Security UserAuthToken
// @Success 200 {object} pkgapp.Res "Success"
// @Router /api/admin/restart [get]
func (h *AdminControlHandler) Restart(c *gin.Context) {
	response := pkgapp.NewResponse(c)
	cfg := h.App.Config()
	uid := pkgapp.GetUID(c)

	if cfg.User.AdminUID != 0 && uid != int64(cfg.User.AdminUID) {
		response.ToResponse(code.ErrorUserIsNotAdmin)
		return
	}

	currentBinary, err := os.Executable()
	if err != nil {
		response.ToResponse(code.Failed.WithDetails("Failed to get current executable path: " + err.Error()))
		return
	}

	h.App.TriggerUpgrade(currentBinary)

	response.ToResponse(code.Success.WithDetails("Restart triggered, server is restarting..."))
}

// GC triggers manual garbage collection and releases memory to OS (requires admin privileges)
// GC 手动触发垃圾回收并释放内存给操作系统（需要管理员权限）
// @Summary Trigger manual GC
// @Description Manually run Go runtime GC and release memory to OS, requires admin privileges
// @Tags System
// @Produce json
// @Security UserAuthToken
// @Success 200 {object} pkgapp.Res "Success"
// @Failure 403 {object} pkgapp.Res "Insufficient privileges"
// @Router /api/admin/gc [get]
func (h *AdminControlHandler) GC(c *gin.Context) {
	response := pkgapp.NewResponse(c)
	cfg := h.App.Config()
	logger := h.App.Logger()

	uid := pkgapp.GetUID(c)
	if uid == 0 {
		response.ToResponse(code.ErrorInvalidUserAuthToken)
		return
	}

	if cfg.User.AdminUID != 0 && uid != int64(cfg.User.AdminUID) {
		response.ToResponse(code.ErrorUserIsNotAdmin)
		return
	}

	var mBefore, mAfter runtime.MemStats
	runtime.ReadMemStats(&mBefore)

	startTime := time.Now()
	// Trigger GC // 触发 GC
	runtime.GC()
	// Release memory to OS // 释放内存给操作系统
	debug.FreeOSMemory()
	duration := time.Since(startTime)

	runtime.ReadMemStats(&mAfter)

	memReleased := int64(mBefore.Alloc) - int64(mAfter.Alloc)
	logger.Info("Manual GC completed",
		zap.Duration("duration", duration),
		zap.Uint64("allocBefore", mBefore.Alloc),
		zap.Uint64("allocAfter", mAfter.Alloc),
		zap.Int64("released", memReleased),
	)

	data := gin.H{
		"duration":    duration.String(),
		"allocBefore": mBefore.Alloc,
		"allocAfter":  mAfter.Alloc,
		"released":    memReleased,
	}

	response.ToResponse(code.Success.WithData(data).WithDetails("Manual GC completed successfully"))
}

// GetWSClients retrieves all currently connected WebSocket clients (requires admin privileges)
// @Summary Get connected WebSocket clients
// @Description Get a list of all current WebSocket connections, requires admin privileges
// @Tags System
// @Security UserAuthToken
// @Produce json
// @Success 200 {object} pkgapp.Res{data=[]pkgapp.WSClientInfo} "Success"
// @Failure 403 {object} pkgapp.Res "Insufficient privileges"
// @Router /api/admin/ws_clients [get]
func (h *AdminControlHandler) GetWSClients(c *gin.Context) {
	response := pkgapp.NewResponse(c)
	cfg := h.App.Config()
	uid := pkgapp.GetUID(c)

	if uid == 0 {
		response.ToResponse(code.ErrorInvalidUserAuthToken)
		return
	}

	if cfg.User.AdminUID != 0 && uid != int64(cfg.User.AdminUID) {
		response.ToResponse(code.ErrorUserIsNotAdmin)
		return
	}

	clients := h.wss.GetClients()
	response.ToResponse(code.Success.WithData(clients))
}

// KickWSClient kicks a WebSocket client (requires admin privileges)
// @Summary Kick a WebSocket client
// @Description Kick a WebSocket client by TraceID, requires admin privileges
// @Tags System
// @Security UserAuthToken
// @Param traceId path string true "Trace ID of the client"
// @Produce json
// @Success 200 {object} pkgapp.Res "Success"
// @Failure 403 {object} pkgapp.Res "Insufficient privileges"
// @Router /api/admin/ws_client/{traceId} [delete]
func (h *AdminControlHandler) KickWSClient(c *gin.Context) {
	response := pkgapp.NewResponse(c)
	cfg := h.App.Config()
	uid := pkgapp.GetUID(c)

	if uid == 0 {
		response.ToResponse(code.ErrorInvalidUserAuthToken)
		return
	}

	if cfg.User.AdminUID != 0 && uid != int64(cfg.User.AdminUID) {
		response.ToResponse(code.ErrorUserIsNotAdmin)
		return
	}

	traceID := c.Param("traceId")
	if traceID == "" {
		response.ToResponse(code.ErrorInvalidParams.WithDetails("traceId is required"))
		return
	}

	if ok := h.wss.KickClient(traceID); !ok {
		response.ToResponse(code.Failed.WithDetails("Client not found or already disconnected"))
		return
	}

	response.ToResponse(code.Success.WithDetails("Client kicked successfully"))
}

// extendWriteDeadline keeps a long upgrade response from being cut off by the
// server's write timeout. A writer that cannot set a deadline is left unchanged;
// the download context is still independent of the request timeout.
func extendWriteDeadline(w http.ResponseWriter, d time.Duration) {
	if w == nil || d <= 0 {
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(d))
}

func (h *AdminControlHandler) extractBinary(tarPath string, destDir string, binaryName string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		// Check if it's the binary we're looking for
		// Often files in tar.gz are in a subdirectory or have different names
		// In alpha-release.yml: tar -czvf ... . (contents of build/platform dir)
		if filepath.Base(header.Name) == binaryName {
			target := filepath.Join(destDir, binaryName)
			f, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR, os.FileMode(header.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
			return nil
		}
	}

	return fmt.Errorf("binary %s not found in archive", binaryName)
}

// CloudflaredTunnelDownload triggers cloudflared binary download (requires admin privileges)
// @Summary Download cloudflared binary
// @Description Trigger the download of cloudflared binary for the current platform
// @Tags System
// @Security UserAuthToken
// @Produce json
// @Success 200 {object} pkgapp.Res "Success"
// @Router /api/admin/cloudflared_tunnel_download [get]
func (h *AdminControlHandler) CloudflaredTunnelDownload(c *gin.Context) {
	response := pkgapp.NewResponse(c)
	cfg := h.App.Config()
	uid := pkgapp.GetUID(c)

	if cfg.User.AdminUID != 0 && uid != int64(cfg.User.AdminUID) {
		response.ToResponse(code.ErrorUserIsNotAdmin)
		return
	}

	h.App.Logger().Info("Starting manual cloudflared binary download via API")

	path, err := h.App.CloudflareService.DownloadBinary()
	if err != nil {
		h.App.Logger().Error("Manual cloudflared download failed", zap.Error(err))
		// 返回详细的错误提示（包含 DownloadBinary 中构造的建议）
		response.ToResponse(code.ErrorCloudflaredDownloadFailed.WithDetails(err.Error()))
		return
	}

	response.ToResponse(code.Success.WithData(gin.H{"path": path}).WithDetails("Cloudflared binary is ready"))
}
