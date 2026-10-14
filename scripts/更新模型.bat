@echo off
chcp 65001 >nul 2>&1
title Vertex AI Proxy - Update Model List
setlocal EnableExtensions

rem =====================================================================
rem  Pull the upstream Google Model Garden catalog and merge it into
rem  config\models.json (add-only; alias_map / family_map are preserved).
rem  Requires only Windows built-in PowerShell (no Python / Node / curl).
rem
rem  NOTE: keep THIS section ASCII-only. cmd.exe mis-parses UTF-8
rem  multibyte characters in a batch file (it desyncs on byte offsets and
rem  reads fragments of lines as commands). All Chinese output therefore
rem  lives in the PowerShell section below, which is read as UTF-8.
rem =====================================================================

rem -- Proxy used to reach upstream. Leave empty to use system proxy / direct.
set "PROXY=http://127.0.0.1:10808"

set "SCRIPT_DIR=%~dp0"
rem Drop the trailing backslash: "C:\path\" would escape the closing quote when
rem passed as an argument to powershell -File.
if "%SCRIPT_DIR:~-1%"=="\" set "SCRIPT_DIR=%SCRIPT_DIR:~0,-1%"

rem -- Extract the PowerShell section (everything after :PS_SCRIPT) to a temp file.
set "PS1=%TEMP%\vproxy_update_models_%RANDOM%%RANDOM%.ps1"
powershell -NoProfile -ExecutionPolicy Bypass -Command "$c=Get-Content -LiteralPath '%~f0' -Encoding UTF8; $i=[array]::IndexOf($c,':PS_SCRIPT'); if($i -lt 0){exit 2}; $c[($i+1)..($c.Count-1)] | Set-Content -LiteralPath '%PS1%' -Encoding UTF8"
if errorlevel 1 (
    echo [X] Failed to extract the embedded PowerShell script. Is PowerShell available?
    pause
    exit /b 1
)

if defined PROXY (
    powershell -NoProfile -ExecutionPolicy Bypass -File "%PS1%" -ScriptDir "%SCRIPT_DIR%" -Proxy "%PROXY%"
) else (
    powershell -NoProfile -ExecutionPolicy Bypass -File "%PS1%" -ScriptDir "%SCRIPT_DIR%"
)
set "RC=%ERRORLEVEL%"

del "%PS1%" >nul 2>&1

echo.
if not "%RC%"=="0" (
    echo [X] Update failed ^(exit code %RC%^). models.json was NOT modified.
    pause
    exit /b %RC%
)
echo [OK] Done. A running service reloads within ~60s, or restart it to apply now.
echo.
pause
exit /b 0

:PS_SCRIPT
# ══════════════════════════════════════════════════════════════════
#  以下为 PowerShell 实现（由上面的 bat 抽取到临时文件后执行）
#  从上游 Google Model Garden 目录拉取模型清单，合并进 config\models.json。
#  只新增、不删除；alias_map / family_map 原样保留。
# ══════════════════════════════════════════════════════════════════
param(
    [Parameter(Mandatory = $true)][string]$ScriptDir,
    [string]$Proxy = ''
)

$ErrorActionPreference = 'Stop'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$AnonKey    = 'AIzaSyCI-zsRP85UVOi0DjtiCwWBwQ1djDy741g'
$Endpoint   = 'https://cloudconsole-pa.clients6.google.com/v3/entityServices/AiplatformEntityService/schemas/AIPLATFORM_GRAPHQL:batchGraphql?key=' + $AnonKey + '&prettyPrint=false'
# ListPublisherModel 的持久化查询签名。Google 滚动更新前端后可能失效，
# 失效时由 Find-FreshSignature 自动重新提取并缓存到 model_catalog_sig.txt。
$DefaultSig = '2/gBk5XPNYZ1xSZpynVBEl2/GnZ5By2c1WQrVqJfS4DFw='
$Parent     = 'publishers/google'
$UserAgent  = 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36'

# 非生成式条目：这些模型不是 generateContent 能服务的，不进模型清单。
$SkipPatterns = @('embedding', '-live', 'live-', 'transcribe', 'robotics', 'computer-use', 'native-audio', '-info')

# ── 路径定位：发布包扁平布局（bat 与 config 同级）优先，其次仓库布局（scripts\..\config）──
function Resolve-ModelsJson([string]$dir) {
    $flat = Join-Path $dir 'config\models.json'
    if (Test-Path -LiteralPath $flat) { return (Resolve-Path -LiteralPath $flat).Path }
    $repo = Join-Path $dir '..\config\models.json'
    if (Test-Path -LiteralPath $repo) { return (Resolve-Path -LiteralPath $repo).Path }
    return ''
}

# ── 手写 2 空格缩进 JSON 序列化：与 Go 侧 json.MarshalIndent(v, "", "  ") 一致，
#    且保持键的原有顺序（PS 5.1 的 ConvertTo-Json 会打乱嵌套哈希表键序）。──
function ConvertTo-JsonString([string]$s) {
    $sb = New-Object System.Text.StringBuilder
    foreach ($ch in $s.ToCharArray()) {
        $c = [string]$ch
        if ($c -eq '"') { [void]$sb.Append('\"') }
        elseif ($c -eq '\') { [void]$sb.Append('\\') }
        elseif ($c -eq "`n") { [void]$sb.Append('\n') }
        elseif ($c -eq "`r") { [void]$sb.Append('\r') }
        elseif ($c -eq "`t") { [void]$sb.Append('\t') }
        elseif ([int]$ch -lt 32) { [void]$sb.Append(('\u{0:x4}' -f [int]$ch)) }
        else { [void]$sb.Append($ch) }
    }
    return '"' + $sb.ToString() + '"'
}

function Add-KVBlock($lines, [string]$name, $pairs, [bool]$isLast) {
    $tail = if ($isLast) { '' } else { ',' }
    if ($pairs.Count -eq 0) {
        [void]$lines.Add('  "' + $name + '": {}' + $tail)
        return
    }
    [void]$lines.Add('  "' + $name + '": {')
    $keys = @($pairs.Keys)
    for ($i = 0; $i -lt $keys.Count; $i++) {
        $comma = if ($i -lt ($keys.Count - 1)) { ',' } else { '' }
        [void]$lines.Add('    ' + (ConvertTo-JsonString $keys[$i]) + ': ' + (ConvertTo-JsonString $pairs[$keys[$i]]) + $comma)
    }
    [void]$lines.Add('  }' + $tail)
}

function ConvertTo-ModelsJson($models, $aliasMap, $familyMap) {
    $lines = New-Object System.Collections.Generic.List[string]
    [void]$lines.Add('{')
    [void]$lines.Add('  "models": [')
    for ($i = 0; $i -lt $models.Count; $i++) {
        $comma = if ($i -lt ($models.Count - 1)) { ',' } else { '' }
        [void]$lines.Add('    ' + (ConvertTo-JsonString $models[$i]) + $comma)
    }
    [void]$lines.Add('  ],')
    Add-KVBlock $lines 'alias_map' $aliasMap $false
    Add-KVBlock $lines 'family_map' $familyMap $true
    [void]$lines.Add('}')
    return ($lines -join "`r`n")
}

# ── 上游拉取 ──
function Invoke-CatalogPage {
    param([string]$Signature, [string]$PageToken)
    $vars = @{ parent = $Parent; pageSize = 100 }
    if ($PageToken) { $vars['pageToken'] = $PageToken }
    $payload = @{
        requestContext = @{
            clientVersion    = 'boq_cloud-boq-clientweb-vertexaistudio_20260630.00_p0'
            pagePath         = '/agent-platform/studio/multimodal'
            pageViewId       = 1234567890123456
            trackingId       = 'd1234567890123456'
            backendOverrides = @{}
            clientSessionId  = '00000000-0000-4000-8000-000000000000'
            selectedPurview  = @{}
            jurisdiction     = 'global'
        }
        querySignature = $Signature
        operationName  = 'ListPublisherModel'
        variables      = $vars
    } | ConvertTo-Json -Depth 12 -Compress

    $reqArgs = @{
        Uri             = $Endpoint
        Method          = 'Post'
        Body            = [Text.Encoding]::UTF8.GetBytes($payload)
        ContentType     = 'application/json'
        UseBasicParsing = $true
        TimeoutSec      = 60
        UserAgent       = $UserAgent
        Headers         = @{
            'Origin'  = 'https://console.cloud.google.com'
            'Referer' = 'https://console.cloud.google.com/'
        }
    }
    if ($Proxy) { $reqArgs['Proxy'] = $Proxy }
    $resp = Invoke-WebRequest @reqArgs
    return ($resp.Content | ConvertFrom-Json)
}

function Get-CatalogError($page) {
    try {
        $errs = $page[0].results[0].errors
        if ($errs -and $errs.Count -gt 0) { return [string]$errs[0].message }
    } catch { }
    return ''
}

function Get-Catalog([string]$Signature) {
    $all   = @()
    $token = ''
    $pages = 0
    while ($true) {
        $page = Invoke-CatalogPage -Signature $Signature -PageToken $token
        $err  = Get-CatalogError $page
        if ($err) { throw $err }
        $node = $page[0].results[0].data.ui.listPublisherModel
        if ($node.data) { $all += $node.data }
        $token = [string]$node.nextPageToken
        $pages++
        if (-not $token -or $pages -ge 30) { break }
    }
    return , $all
}

# 签名失效时：抓控制台页面 → 定位 AiPlatformMicroUi bundle → 正则提取新签名。
function Find-FreshSignature {
    Write-Host '[!] 缓存/内置签名已失效，尝试从上游前端 bundle 重新提取...'
    $sess = New-Object Microsoft.PowerShell.Commands.WebRequestSession
    $page = Invoke-WebRequest -Uri 'https://console.cloud.google.com/agent-platform/studio/multimodal' `
        -UseBasicParsing -TimeoutSec 60 -UserAgent $UserAgent -WebSession $sess `
        -Headers @{ 'Accept' = 'text/html,application/xhtml+xml'; 'Accept-Language' = 'zh-CN,zh;q=0.9' }
    $html = $page.Content -replace '\\{1,2}u003d', '=' -replace '\\{1,2}u0026', '&'
    $m = [regex]::Match($html, "https://www\.gstatic\.com/_/mss/boq-cloud-client/_/js/k=boq-cloud-client\.AiPlatformMicroUi[^`"'\\ ]+")
    if (-not $m.Success) { throw '未能在控制台页面定位 AiPlatformMicroUi bundle' }
    Write-Host ('[i] bundle：' + $m.Value)
    $js  = (Invoke-WebRequest -Uri $m.Value -UseBasicParsing -TimeoutSec 180 -UserAgent $UserAgent).Content
    # 锚定 request("ListPublisherModel", —— 直接匹配操作名会命中它的前缀
    # ListPublisherModelRegional（另一个操作，签名不同）。惰性匹配取最近的 @Signature。
    $m2  = [regex]::Match($js, 'request\("ListPublisherModel",[^\r\n]{0,4000}?@Signature\(bytes:\s*"([^"]+)"\)')
    if (-not $m2.Success) { throw '未能在 bundle 中提取 ListPublisherModel 签名' }
    return $m2.Groups[1].Value
}

try {
    Write-Host ''
    Write-Host '╔══════════════════════════════════════════════════╗'
    Write-Host '║   Vertex AI Proxy — 更新模型列表                 ║'
    Write-Host '╚══════════════════════════════════════════════════╝'
    Write-Host ''

    $ModelsJson = Resolve-ModelsJson $ScriptDir
    if (-not $ModelsJson) {
        throw '未找到 config\models.json。请把本脚本放在程序目录（与 vertex-proxy.exe 同级）或仓库的 scripts 目录下运行。'
    }
    Write-Host ('[OK] 目标文件：' + $ModelsJson)
    if ($Proxy) { Write-Host ('[i] 拉取代理：' + $Proxy) } else { Write-Host '[i] 拉取代理：系统默认 / 直连' }
    Write-Host ''

    $SigFile  = Join-Path (Split-Path -Parent $ModelsJson) 'model_catalog_sig.txt'
    $existing = (Get-Content -LiteralPath $ModelsJson -Raw -Encoding UTF8) | ConvertFrom-Json

    $models    = @($existing.models)
    $origCount = $models.Count

    # 用有序表承接，保持原文件里的键序（供 JSON 序列化与 diff 稳定）。
    $aliasMap  = [ordered]@{}
    if ($existing.alias_map) {
        $existing.alias_map.PSObject.Properties | ForEach-Object { $aliasMap[$_.Name] = $_.Value }
    }
    $familyMap = [ordered]@{}
    if ($existing.family_map) {
        $existing.family_map.PSObject.Properties | ForEach-Object { $familyMap[$_.Name] = $_.Value }
    }
    $origAlias = $aliasMap.Count

    $recover = $false
    if ($origCount -eq 0) {
        Write-Host '[!] models.json 的 models 为空（服务当前会回退到内置默认清单），将按上游目录重建。'
        $recover = $true
    }

    # ── 拉取（签名失效时自愈一次）──
    $sig = $DefaultSig
    if (Test-Path -LiteralPath $SigFile) {
        $cached = (Get-Content -LiteralPath $SigFile -Raw -Encoding UTF8)
        if ($cached) { $cached = $cached.Trim() }
        if ($cached) { $sig = $cached }
    }
    try {
        $catalog = Get-Catalog -Signature $sig
    } catch {
        $msg = $_.Exception.Message
        if (($msg -match 'signature') -or ($msg -match 'not found') -or ($msg -match 'not be found')) {
            $sig = Find-FreshSignature
            try { [IO.File]::WriteAllText($SigFile, $sig, (New-Object Text.UTF8Encoding($false))) } catch { }
            $catalog = Get-Catalog -Signature $sig
        } else {
            throw
        }
    }
    Write-Host ('[OK] 上游目录拉取成功，共 ' + $catalog.Count + ' 条')

    $candidates = $catalog |
        ForEach-Object { $_.name -replace '^publishers/google/models/', '' } |
        Where-Object { $_ -like 'gemini-*' } |
        Where-Object {
            $n = $_
            $skip = $false
            foreach ($p in $SkipPatterns) { if ($n -like ('*' + $p + '*')) { $skip = $true; break } }
            -not $skip
        } |
        Sort-Object -Unique
    Write-Host ('[i] 生成式候选：' + $candidates.Count + ' 条')

    # ── 合并：只新增不删除 ──
    $added = @()
    if ($recover) {
        $models = @($candidates)
        $added  = @($candidates)
    } else {
        foreach ($m in $candidates) {
            if ($models -notcontains $m) { $models += $m; $added += $m }
        }
    }

    # 新出现的 nano-banana 系生图模型名称不含 image，自动补家族声明避免被误判为文本。
    foreach ($m in $models) {
        if (($m -like '*nano-banana*') -and (-not $familyMap.Contains($m))) {
            $familyMap[$m] = 'image'
            Write-Host ('[i] 自动声明家族：' + $m + ' = image')
        }
    }

    # ── 写盘前自检（加载器对坏文件是静默回退到内置清单，必须在这里拦住）──
    if ($models.Count -eq 0) { throw '合并后模型清单为空，已中止（未写盘）' }
    if ((-not $recover) -and ($models.Count -lt $origCount)) {
        throw ('模型数减少（' + $origCount + ' → ' + $models.Count + '），已中止（未写盘）')
    }
    if ($aliasMap.Count -ne $origAlias) { throw '别名表条目数变化，已中止（未写盘）' }

    # ── 备份 + 原子写 ──
    Copy-Item -LiteralPath $ModelsJson -Destination ($ModelsJson + '.bak') -Force
    $json = ConvertTo-ModelsJson $models $aliasMap $familyMap
    $tmp  = $ModelsJson + '.tmp'
    [IO.File]::WriteAllText($tmp, $json, (New-Object Text.UTF8Encoding($false)))
    Move-Item -LiteralPath $tmp -Destination $ModelsJson -Force

    # ── 汇总 ──
    Write-Host ''
    Write-Host ('[OK] 已写入：' + $ModelsJson)
    Write-Host ('     模型 ' + $models.Count + ' 条（原 ' + $origCount + '），新增 ' + $added.Count + ' 条')
    foreach ($m in $added) { Write-Host ('     + ' + $m) }
    Write-Host ('     别名 ' + $aliasMap.Count + ' 条，家族覆盖 ' + $familyMap.Count + ' 条')
    Write-Host ('     备份：' + $ModelsJson + '.bak')
    exit 0
} catch {
    Write-Host ''
    Write-Host ('[X] ' + $_.Exception.Message) -ForegroundColor Red
    exit 1
}
