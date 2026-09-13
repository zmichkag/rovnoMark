param(
    [Parameter(Mandatory = $true)]
    [string]$Device,
    [string[]]$Fields = @('GL19', 'GL4A', 'GL4B', 'GL4C', 'GT03', 'GT04', 'GT51', 'GT52', 'GT53')
)

$ErrorActionPreference = 'Stop'
$communication = New-Object -ComObject 'BCS.BCSComunnication'
$opened = $false

try {
    $identity = "rovnoMark-read-fields-$PID"
    $openResult = $communication.Open($identity, $Device, 0, 0, 0)
    if ($openResult -ne 0) {
        throw "BCS Open failed: device=$Device result=$openResult"
    }
    $opened = $true

    foreach ($field in $Fields) {
        $handle = ''
        $sendStatus = 0
        $sendResult = $communication.Send("A?$field", '0', [ref]$handle, 5000, [ref]$sendStatus)
        if ($sendResult -ne 0 -or ($sendStatus -ne 0 -and $sendStatus -ne 2)) {
            Write-Output "$Device`t$field`tSEND_ERROR result=$sendResult status=$sendStatus"
            continue
        }

        $packet = ''
        $receiveStatus = 0
        $received = $false
        for ($attempt = 0; $attempt -lt 5; $attempt++) {
            $receiveResult = $communication.ReceiveOne([ref]$packet, $handle, 1000, [ref]$receiveStatus)
            if ($receiveResult -ne 0) {
                break
            }
            if ($receiveStatus -ne 1) {
                $received = $true
                break
            }
        }
        if ($received) {
            Write-Output "$Device`t$field`tstatus=$receiveStatus`t$packet"
        } else {
            Write-Output "$Device`t$field`tRECEIVE_ERROR result=$receiveResult status=$receiveStatus handle=$handle"
        }
    }
}
finally {
    if ($opened) {
        [void]$communication.Close()
    }
    [void][System.Runtime.InteropServices.Marshal]::FinalReleaseComObject($communication)
}
