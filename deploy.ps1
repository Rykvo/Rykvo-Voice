param(
    [string]$Server,
    [string]$User = 'root',
    [ValidateRange(1, 65535)][int]$Port = 22,
    [ValidateSet('install', 'update', 'uninstall', 'status')][string]$Action
)
$ErrorActionPreference = 'Stop'
if (-not $Server) { $Server = Read-Host '服务器 IP 或主机名' }
if ($Server -notmatch '^[a-zA-Z0-9][a-zA-Z0-9.-]*$' -or $User -notmatch '^[a-zA-Z_][a-zA-Z0-9_-]*$') {
    throw '服务器或用户名格式无效'
}
if (-not $Action) {
    Write-Host "1. 安装`n2. 更新`n3. 卸载（保留数据）`n4. 状态"
    $Action = @{ '1' = 'install'; '2' = 'update'; '3' = 'uninstall'; '4' = 'status' }[(Read-Host '选择')]
    if (-not $Action) { throw '选择无效' }
}
Get-Command ssh, scp, gh -ErrorAction Stop | Out-Null
$Target = "$User@$Server"
$Prefix = if ($User -eq 'root') { '' } else { 'sudo ' }
if ($Action -in 'uninstall', 'status') {
    & ssh -t -p $Port $Target "${Prefix}rykvo $Action"
    if ($LASTEXITCODE -ne 0) { throw '远程操作未完成' }
    exit
}
$Id = [Guid]::NewGuid().ToString('N')
$Local = Join-Path ([IO.Path]::GetTempPath()) "rykvo-download-$Id"
$Remote = "/var/tmp/rykvo-upload-$Id"
New-Item -ItemType Directory -Path $Local | Out-Null
try {
    $Architecture = (& ssh -p $Port $Target "umask 077; mkdir '$Remote'; uname -m").Trim()
    if ($LASTEXITCODE -ne 0) { throw 'SSH 连接失败' }
    $Arch = @{ 'x86_64' = 'amd64'; 'aarch64' = 'arm64'; 'arm64' = 'arm64' }[$Architecture]
    if (-not $Arch) { throw '支持 amd64/arm64' }
    $Asset = "rykvo-voice-linux-$Arch.tar.gz"
    & gh release download --repo Rykvo/Rykvo-Voice --pattern $Asset --pattern release.json --dir $Local
    if ($LASTEXITCODE -ne 0) { throw '请先 gh auth login，并确认仓库读取权限' }
    $Manifest = Get-Content -LiteralPath (Join-Path $Local 'release.json') -Raw | ConvertFrom-Json
    $Expected = $Manifest.assets.$Arch.sha256
    $Archive = Join-Path $Local $Asset
    $Digest = (Get-FileHash -LiteralPath $Archive -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($Expected -notmatch '^[a-f0-9]{64}$' -or $Digest -ne $Expected) { throw '安装包校验失败' }
    & scp -P $Port $Archive "${Target}:$Remote/bundle.tar.gz"
    if ($LASTEXITCODE -ne 0) { throw '上传失败' }
    $Command = "set -eu; cd '$Remote'; printf '%s  %s\n' '$Digest' bundle.tar.gz | sha256sum -c -; tar --no-same-owner -xzf bundle.tar.gz; ${Prefix}bash install.sh $Action"
    & ssh -t -p $Port $Target $Command
    if ($LASTEXITCODE -ne 0) { throw '部署未完成，请查看远程提示' }
} finally {
    # 只移除本次两个已知下载文件；非空目录保留供排查。
    foreach ($Name in @('release.json', 'rykvo-voice-linux-amd64.tar.gz', 'rykvo-voice-linux-arm64.tar.gz')) {
        $File = Join-Path $Local $Name
        if (Test-Path -LiteralPath $File -PathType Leaf) { Remove-Item -LiteralPath $File -Force }
    }
    if (-not (Get-ChildItem -LiteralPath $Local -Force)) { Remove-Item -LiteralPath $Local }
}
