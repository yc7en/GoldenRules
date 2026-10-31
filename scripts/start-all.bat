@echo off
setlocal
rem 三个服务各开一个窗口。先引擎后平台：平台启动后第一次发布要做门禁校验，
rem 引擎没起来时发布会被 fail-closed 拦下，报错长得像权限问题。
rem 本文件是 GBK 编码、CRLF 换行，勿改 UTF-8。
cd /d "%~dp0.."
set "ROOT=%CD%"

start "GoldenRules 决策服务" "%ROOT%\scripts\start-engine.bat"
timeout /t 2 /nobreak >nul 2>&1
start "GoldenRules 管理平台" "%ROOT%\scripts\start-platform.bat"
timeout /t 2 /nobreak >nul 2>&1
start "GoldenRules 运营台" "%ROOT%\scripts\start-web.bat"

echo 已打开三个窗口
echo 决策服务 http://127.0.0.1:18080/healthz
echo 管理平台 http://127.0.0.1:18081/healthz
echo 运营台 http://localhost:5173
echo.
echo 关掉对应窗口即停止该服务；要一次全停用 scripts\stop-all.bat
timeout /t 5 /nobreak >nul 2>&1
