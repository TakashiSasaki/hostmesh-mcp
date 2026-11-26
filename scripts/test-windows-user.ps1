# Run on a disposable Windows CI runner after building the peer.
$ErrorActionPreference = 'Stop'
$user = 'HostMeshTest'
$password = ConvertTo-SecureString ("Aa1!" + [Guid]::NewGuid().ToString('N')) -AsPlainText -Force
$account = New-LocalUser -Name $user -Password $password
$work = Join-Path $env:PUBLIC ("hostmesh-" + [Guid]::NewGuid().ToString('N'))
try {
    if ($account.SID -notin (Get-LocalGroupMember -SID 'S-1-5-32-545').SID) {
        Add-LocalGroupMember -SID 'S-1-5-32-545' -Member $account
    }
    New-Item -ItemType Directory -Path $work | Out-Null
    icacls $work /grant "${user}:(OI)(CI)M" | Out-Null
    if ($LASTEXITCODE) { throw "Cannot grant test directory access" }
    Copy-Item "$env:RUNNER_TEMP/hostmesh-mcp.exe" $work
    go test -c -o "$work/probe.exe" ./cmd/hostmesh-mcp
    if ($LASTEXITCODE) { throw "Cannot compile test" }
    @'
@echo off
set "TEMP=%~dp0"
set "TMP=%~dp0"
set "HOST_STATUS_TEST_BINARY=%~dp0hostmesh-mcp.exe"
set "HOST_STATUS_STANDARD_USER=1"
"%~dp0probe.exe" -test.v -test.run "^Test(WindowsNative|Stdio|CABootstrap)$" > "%~dp0result.log" 2>&1
exit /b %errorlevel%
'@ | Set-Content "$work/run.cmd" -Encoding ascii
    $cred = [pscredential]::new("$env:COMPUTERNAME\$user", $password)
    $run = Start-Process $env:ComSpec -ArgumentList '/d','/c','run.cmd' -WorkingDirectory $work -Credential $cred -LoadUserProfile -Wait -PassThru
    Get-Content "$work/result.log"
    if ($run.ExitCode) { throw "Standard-user test failed ($($run.ExitCode))" }
} finally {
    Remove-LocalUser -Name $user
    if (Test-Path $work) { Remove-Item -LiteralPath $work -Recurse -Force }
}
