@echo off
setlocal EnableDelayedExpansion
chcp 65001 >nul
title WeChat Profile Bot
cd /d "%~dp0"

set "BOT_EXE=wechat-profile-bot-windows-amd64.exe"
if not exist "!BOT_EXE!" set "BOT_EXE=wechat-profile-bot.exe"
if not exist "!BOT_EXE!" (
    echo [错误] 未找到可执行程序 wechat-profile-bot-windows-amd64.exe（也未找到 wechat-profile-bot.exe）
    echo   请先编译或从 Releases 下载 Windows 版二进制，放到本目录后重试：
    echo   目录: %~dp0
    echo   编译: go build -o wechat-profile-bot-windows-amd64.exe .
    echo.
    pause
    exit /b 1
)

echo ============================================
echo   WeChat Profile Bot - 前台运行
echo   按 Ctrl+C 停止（已登录凭据会保留）
echo   首次扫码登录请在本窗口完成，二维码链接不会写入日志文件
echo ============================================
echo.
"!BOT_EXE!"
endlocal
pause
