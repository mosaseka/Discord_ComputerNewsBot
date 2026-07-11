[CmdletBinding()]
param(
    [string]$Executable = "",

    [string[]]$BotArguments = @()
)

$ErrorActionPreference = "Stop"
$repoRoot = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot ".."))

if ([string]::IsNullOrWhiteSpace($Executable)) {
    $Executable = Join-Path $repoRoot "bin\discord-news-bot.exe"
}
elseif (-not [System.IO.Path]::IsPathRooted($Executable)) {
    $Executable = Join-Path $repoRoot $Executable
}

if (-not (Test-Path -LiteralPath $Executable -PathType Leaf)) {
    throw "Bot executable not found: $Executable. Build it with: go build -trimpath -o .\bin\discord-news-bot.exe .\cmd\bot"
}

$logDirectory = Join-Path $repoRoot "logs"
New-Item -ItemType Directory -Force -Path $logDirectory | Out-Null
$logPath = Join-Path $logDirectory ("bot-{0}.log" -f (Get-Date -Format "yyyy-MM-dd"))

Push-Location -LiteralPath $repoRoot
try {
    Add-Content -LiteralPath $logPath -Encoding UTF8 -Value ("{0} launcher_start executable={1}" -f (Get-Date -Format "o"), $Executable)
    & $Executable @BotArguments *>> $logPath
    $exitCode = $LASTEXITCODE
    Add-Content -LiteralPath $logPath -Encoding UTF8 -Value ("{0} launcher_exit code={1}" -f (Get-Date -Format "o"), $exitCode)
    exit $exitCode
}
finally {
    Pop-Location
}
