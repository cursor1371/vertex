@echo off
chcp 65001 >nul 2>&1
title Vertex AI Proxy - Compact Database
setlocal EnableExtensions

rem =====================================================================
rem  Offline database compaction: rebuilds data.db to reclaim free-list
rem  pages (SQLite never shrinks the file on DELETE) and switches the new
rem  file to auto_vacuum=INCREMENTAL so future cleanup reclaims space.
rem
rem  Requires the service to be STOPPED. Uses only the shipped
rem  vertex-proxy.exe `vacuum` subcommand - no sqlite3 / Python needed.
rem
rem  NOTE: keep THIS section ASCII-only. cmd.exe mis-parses UTF-8
rem  multibyte characters in a batch file. Chinese output lives in the
rem  PowerShell section below, which is read as UTF-8.
rem =====================================================================

set "SCRIPT_DIR=%~dp0"
rem Drop the trailing backslash: "C:\path\" would escape the closing quote.
if "%SCRIPT_DIR:~-1%"=="\" set "SCRIPT_DIR=%SCRIPT_DIR:~0,-1%"

set "PS1=%TEMP%\vproxy_compact_db_%RANDOM%%RANDOM%.ps1"
powershell -NoProfile -ExecutionPolicy Bypass -Command "$c=Get-Content -LiteralPath '%~f0' -Encoding UTF8; $i=[array]::IndexOf($c,':PS_SCRIPT'); if($i -lt 0){exit 2}; $c[($i+1)..($c.Count-1)] | Set-Content -LiteralPath '%PS1%' -Encoding UTF8"
if errorlevel 1 (
    echo [X] Failed to extract the embedded PowerShell script. Is PowerShell available?
    pause
    exit /b 1
)

powershell -NoProfile -ExecutionPolicy Bypass -File "%PS1%" -ScriptDir "%SCRIPT_DIR%"
set "RC=%ERRORLEVEL%"

del "%PS1%" >nul 2>&1

echo.
if not "%RC%"=="0" (
    echo [X] Compaction did not complete ^(exit code %RC%^). Your database was not modified.
)
pause
exit /b %RC%

:PS_SCRIPT
# ══════════════════════════════════════════════════════════════════
#  以下为 PowerShell 实现（由上面的 bat 抽取到临时文件后执行）
# ══════════════════════════════════════════════════════════════════
param(
    [Parameter(Mandatory = $true)][string]$ScriptDir
)

$ErrorActionPreference = 'Stop'

function Find-Exe([string]$dir) {
    foreach ($c in @((Join-Path $dir 'vertex-proxy.exe'), (Join-Path $dir '..\vertex-proxy.exe'))) {
        if (Test-Path -LiteralPath $c) { return (Resolve-Path -LiteralPath $c).Path }
    }
    return ''
}

Write-Host ''
Write-Host '╔══════════════════════════════════════════════════════╗'
Write-Host '║   Vertex AI Proxy — 数据库离线压缩                   ║'
Write-Host '╚══════════════════════════════════════════════════════╝'
Write-Host ''

$exe = Find-Exe $ScriptDir
if (-not $exe) {
    Write-Host '[X] 未找到 vertex-proxy.exe（请把它与本脚本放在同一目录）。'
    exit 1
}
Write-Host ('[OK] 主程序：' + $exe)

# 探测二进制是否带 vacuum 子命令：新版 version 会立刻退出；旧版会忽略参数
# 直接启动服务，因此用超时判定并杀掉，避免误把服务拉起来。
# 注意：Start-Process 不接受 stdout/stderr 指向同一个文件，须分两个。
$probeOut = [IO.Path]::GetTempFileName()
$probeErr = [IO.Path]::GetTempFileName()
$ok = $false
try {
    $p = Start-Process -FilePath $exe -ArgumentList 'version' -NoNewWindow -PassThru `
        -RedirectStandardOutput $probeOut -RedirectStandardError $probeErr
    if ($p.WaitForExit(8000)) { $ok = $true } else { try { $p.Kill() } catch { } }
} catch {
    Write-Host ('[X] 无法运行主程序：' + $_.Exception.Message)
    Remove-Item -LiteralPath $probeOut, $probeErr -Force -ErrorAction SilentlyContinue
    exit 1
}
$verText = ''
foreach ($f in @($probeOut, $probeErr)) {
    if (Test-Path -LiteralPath $f) {
        $t = (Get-Content -LiteralPath $f -Raw -Encoding UTF8)
        if ($t) { $verText += $t }
        Remove-Item -LiteralPath $f -Force -ErrorAction SilentlyContinue
    }
}
if (-not $ok) {
    Write-Host '[X] 当前 vertex-proxy.exe 不支持 vacuum 子命令（这是旧版本）。'
    Write-Host '    请先用新版源码重新编译，再用本脚本压缩数据库。'
    exit 1
}
if ($verText) { Write-Host ('[i] ' + $verText.Trim()) }

Write-Host ''
Write-Host '  ⚠️  本操作必须在【服务已停止】时进行。'
Write-Host '      数据库被占用时脚本会直接报错退出，不会改动任何文件。'
Write-Host ''
$ans = Read-Host '  服务是否已停止？输入 y 继续，其他任意键退出'
if ($ans -ne 'y' -and $ans -ne 'Y') {
    Write-Host '  已取消。'
    exit 0
}
Write-Host ''

Push-Location (Split-Path -Parent $exe)
try {
    & $exe vacuum
    $code = $LASTEXITCODE
} finally {
    Pop-Location
}

if ($code -ne 0) { exit $code }

Write-Host '  下一步：'
Write-Host '    1) 启动 vertex-proxy；'
Write-Host '    2) 打开后台「数据库」页，确认体积与表行数正常；'
Write-Host '    3) 确认无误后删除 config\data.db.old 释放空间。'
Write-Host ''
exit 0
