param(
    [string]$Device = 'GLPMax',
    [string]$MarksFile = (Join-Path $PSScriptRoot 'test-marks.txt')
)

$ErrorActionPreference = 'Stop'

function Get-BcsErrorText {
    param($Communication)

    $errorNumber = 0
    $systemNumber = 0
    $errorText = ''
    $Communication.Error([ref]$errorNumber, [ref]$systemNumber, [ref]$errorText)
    return "error=$errorNumber system=$systemNumber text=$errorText"
}

function Send-BcsMessage {
    param(
        $Communication,
        [string]$Header,
        [string]$Data
    )

    $handle = ''
    $status = 0
    $result = $Communication.Send($Header, $Data, [ref]$handle, 30000, [ref]$status)
    if ($result -ne 0 -or $status -ne 0) {
        throw "COM Send $Header failed: result=$result status=$status handle=$handle; $(Get-BcsErrorText $Communication)"
    }
    Write-Output "SEND header=$Header data=$Data handle=$handle"
}

function Convert-Mark {
    param([string]$Raw)

    if ($Raw.Length -le 18 -or -not $Raw.StartsWith('01') -or $Raw.Substring(16, 2) -ne '21') {
        throw "Invalid GS1 mark: $Raw"
    }

    $separator = $Raw.IndexOf('<GS>', 18)
    if ($separator -lt 0) {
        throw "Missing <GS> in mark: $Raw"
    }

    $serial = $Raw.Substring(18, $separator - 18)
    $tail = $Raw.Substring($separator + 4)
    if ([string]::IsNullOrWhiteSpace($serial) -or -not $tail.StartsWith('93')) {
        throw "Invalid serial or AI 93 in mark: $Raw"
    }

    return [PSCustomObject]@{
        Raw = $Raw
        GT03 = $serial
        GT04 = '@1D' + $tail
    }
}

function Set-Mark {
    param($Communication, $Mark, [int]$Index, [int]$Count)

    Send-BcsMessage $Communication 'A!GT03' $Mark.GT03
    Send-BcsMessage $Communication 'A!GT04' $Mark.GT04
    Write-Output "MARK installed=$Index/$Count GT03=$($Mark.GT03) GT04=$($Mark.GT04)"
}

function Format-CodeUnits {
    param([string]$Value)

    return (($Value.ToCharArray() | ForEach-Object { '{0:X4}' -f [int]$_ }) -join ' ')
}

$marks = @(Get-Content -LiteralPath $MarksFile | ForEach-Object { $_.Trim() } | Where-Object { $_ } | ForEach-Object { Convert-Mark $_ })
if ($marks.Count -eq 0) {
    throw "Marks file is empty: $MarksFile"
}

$communication = New-Object -ComObject 'BCS.BCSComunnication'
$queue = 'DUSTBIN'
$opened = $false
$channelEOpened = $false

try {
    $identity = "rovnoMark-com-cycle-$PID"
    $openResult = $communication.Open($identity, $Device, 1, 0, 0)
    if ($openResult -ne 0) {
        throw "COM Open failed: result=$openResult; $(Get-BcsErrorText $communication)"
    }
    $opened = $true
    Write-Output "OPEN device=$Device identity=$identity"

    Send-BcsMessage $communication 'A!GWC3' '1'
    $channelEOpened = $true
    Write-Output 'CHANNEL E opened'

    Write-Output "QUEUE using=$queue"

    Set-Mark $communication $marks[0] 1 $marks.Count
    $printed = 0

    Write-Output 'WAITING for COM packets...'
    while ($true) {
        $packet = ''
        $status = 0
        $receiveResult = $communication.ReceiveOne([ref]$packet, $queue, 1000, [ref]$status)
        if ($receiveResult -ne 0) {
            throw "ReceiveOne failed: result=$receiveResult status=$status; $(Get-BcsErrorText $communication)"
        }
        if ($status -eq 1) {
            continue
        }
        if ($status -ne 0) {
            Write-Output "RECEIVE status=$status (no packet processed)"
            continue
        }

        $timestamp = Get-Date -Format 'yyyy-MM-dd HH:mm:ss.fff'
        Write-Output "PACKET time=$timestamp queue=$queue length=$($packet.Length)"
        Write-Output "PACKET text=$packet"
        Write-Output "PACKET utf16-code-units=$(Format-CodeUnits $packet)"

        $isPackage = $packet -match '(?:^A[!?]|\|)PV(?:01|04|05|06)(?:\||$)'
        if (-not $isPackage) {
            Write-Output 'PACKET is not PV01/PV04/PV05/PV06; mark unchanged'
            continue
        }

        $next = $printed + 1
        if ($next -lt $marks.Count) {
            Set-Mark $communication $marks[$next] ($next + 1) $marks.Count
        }
        else {
            Write-Output 'MARKS exhausted; keeping the last mark installed'
        }

        $printed++
        Write-Output "PACKAGE completed=$printed/$($marks.Count); ReceiveOne acknowledged it"
    }
}
finally {
    if ($opened -and $channelEOpened) {
        try {
            Send-BcsMessage $communication 'A!GWC3' '0'
            Write-Output 'CHANNEL E closed'
        }
        catch {
            Write-Output "CHANNEL E close failed: $($_.Exception.Message)"
        }
    }
    if ($opened) {
        try { $null = $communication.Close() } catch {}
    }
    [Runtime.InteropServices.Marshal]::FinalReleaseComObject($communication) | Out-Null
}
