param(
    [string]$MoviesRoot,
    [string]$TVRoot,
    [string]$CouchUrl = "https://couch.dsp.coffee/stream",
    [ValidateRange(1, 12)]
    [int]$LookAhead = 3,
    [ValidateRange(3, 300)]
    [int]$PollSeconds = 10
)

$ErrorActionPreference = "Stop"
$CouchUrl = $CouchUrl.TrimEnd("/")

function Resolve-MediaRoot([string]$ConfiguredPath, [string]$ShareName) {
    if ($ConfiguredPath) {
        return (Resolve-Path -LiteralPath $ConfiguredPath).Path
    }
    try {
        $share = Get-SmbShare -Name $ShareName -ErrorAction Stop
        return (Resolve-Path -LiteralPath $share.Path).Path
    }
    catch {
        throw "Could not find the $ShareName share. Run again with its local folder path."
    }
}

$roots = @{
    "movies" = Resolve-MediaRoot $MoviesRoot "OwncastMovies"
    "tv" = Resolve-MediaRoot $TVRoot "OwncastTV"
}

$secureToken = Read-Host "Couch manager password" -AsSecureString
$credential = [System.Management.Automation.PSCredential]::new("couch", $secureToken)
$token = $credential.GetNetworkCredential().Password
$headers = @{ Authorization = "Bearer $token" }

Write-Host "Watching the Couch playlist. Keep this window open while streaming."

while ($true) {
    try {
        $queue = @(Invoke-RestMethod -Uri "$CouchUrl/api/cache/queue?lookahead=$LookAhead" -Headers $headers -Method Get)
        $pending = @($queue | Where-Object { $_.status.state -ne "cached" })

        foreach ($item in $pending) {
            $share = ([string]$item.share).ToLowerInvariant()
            if (-not $roots.ContainsKey($share)) {
                Write-Warning "No local folder is configured for share '$share'."
                continue
            }
            $relative = ([string]$item.path).Replace("/", [IO.Path]::DirectorySeparatorChar)
            $source = Join-Path -Path $roots[$share] -ChildPath $relative
            if (-not (Test-Path -LiteralPath $source -PathType Leaf)) {
                Write-Warning "File not found: $source"
                continue
            }

            $name = Split-Path -Leaf $source
            Write-Host "Sending $name to Couch..."
            $escapedUrl = [Uri]::EscapeDataString([string]$item.url)
            Invoke-RestMethod -Uri "$CouchUrl/api/cache/upload?url=$escapedUrl" -Headers $headers -Method Put -InFile $source -ContentType "application/octet-stream" | Out-Null
            Write-Host "Cached: $name"
        }
    }
    catch {
        Write-Warning $_.Exception.Message
    }

    Start-Sleep -Seconds $PollSeconds
}
