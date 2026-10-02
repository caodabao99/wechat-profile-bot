@echo off
setlocal EnableDelayedExpansion
chcp 65001 >nul
title 安装 WeChat Profile Bot Windows 服务
cd /d "%~dp0"

set "SERVICE_NAME=WeChatProfileBot"

echo ============================================
echo   安装 WeChat Profile Bot 为 Windows 服务
echo ============================================
echo.

REM 检查 nssm.exe 是否在同目录
if not exist "nssm.exe" (
    echo [错误] 未找到 nssm.exe，请先下载 nssm：
    echo   1. 访问 https://nssm.cc/download
    echo   2. 解压后将 nssm.exe（win64 目录下）放到本程序目录
    echo   3. 重新运行本脚本
    echo.
    pause
    exit /b 1
)

REM 检查程序是否存在
set "BOT_EXE=wechat-profile-bot-windows-amd64.exe"
if not exist "!BOT_EXE!" set "BOT_EXE=wechat-profile-bot.exe"
if not exist "!BOT_EXE!" (
    echo [错误] 未找到可执行程序 wechat-profile-bot-windows-amd64.exe（也未找到 wechat-profile-bot.exe）
    echo   请先编译或从 Releases 下载 Windows 版二进制，放到本目录后重试：
    echo   目录: %~dp0
    echo.
    pause
    exit /b 1
)

REM 检查服务是否已存在（已存在则要求先卸载，避免直接覆盖旧配置）
sc query !SERVICE_NAME! >nul 2>&1
if errorlevel 1 goto install_service
echo [提示] 服务 !SERVICE_NAME! 已存在，请先运行 uninstall-service.bat 卸载，再重新执行本脚本。
echo.
pause
exit /b 1

:install_service
echo 正在安装服务...
nssm.exe install !SERVICE_NAME! "%~dp0!BOT_EXE!"
nssm.exe set !SERVICE_NAME! AppDirectory "%~dp0"
nssm.exe set !SERVICE_NAME! AppStdout "%~dp0bot.log"
nssm.exe set !SERVICE_NAME! AppStderr "%~dp0bot-error.log"
nssm.exe set !SERVICE_NAME! Start SERVICE_AUTO_START
nssm.exe set !SERVICE_NAME! Description "WeChat Profile Bot - 微信画像机器人服务端"

REM 日志轮转：单文件超过 10MB 自动切割，重启后追加而不是覆盖历史日志
nssm.exe set !SERVICE_NAME! AppRotateFiles 1
nssm.exe set !SERVICE_NAME! AppRotateOnline 1
nssm.exe set !SERVICE_NAME! AppRotateBytes 10485760
nssm.exe set !SERVICE_NAME! AppStdoutCreationDisposition 4
nssm.exe set !SERVICE_NAME! AppStderrCreationDisposition 4

REM 重启节流：首次启动若缺少 config.json，程序会写完模板后退出，
REM 这里限制重启频率，避免服务被无限快速重启刷爆日志
nssm.exe set !SERVICE_NAME! AppThrottle 5000
nssm.exe set !SERVICE_NAME! AppExit Default Restart
nssm.exe set !SERVICE_NAME! AppRestartDelay 10

echo.
echo ============================================
echo   服务安装完成
echo ============================================
echo   服务名: !SERVICE_NAME!
echo   工作目录: %~dp0
echo   日志文件: bot.log / bot-error.log（单文件超 10MB 自动轮转）
echo.
echo   启动服务: net start !SERVICE_NAME!
echo   停止服务: net stop !SERVICE_NAME!
echo   卸载服务: uninstall-service.bat
echo.
echo   首次扫码绑定（推荐顺序，更安全）：
echo   1. 先运行 start.bat，在前台窗口完成首次扫码登录，
echo      二维码链接直接打印在控制台，不会写入日志文件
echo   2. 登录成功后凭据会保存到 ilink_credentials.json
echo   3. 关闭 start.bat 窗口，再执行 net start !SERVICE_NAME! 常驻运行
echo.
echo   [安全提示] bot.log 中可能出现登录链接，它等同于登录凭据，属敏感信息：
echo     - 请勿外传、截图，也不要提交到版本库
echo     - 如需清理，直接删除 bot.log / bot-error.log 即可
echo.

REM 询问是否立即启动
set /p startnow=是否立即启动服务？(y/n):
if /i "!startnow!"=="y" (
    net start !SERVICE_NAME!
)

endlocal
pause
