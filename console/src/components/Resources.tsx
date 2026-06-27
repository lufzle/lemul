import { type ResourceUsage } from '#/lib/control-plane'
import { Bar, DualLine, Panel, Sparkline, StackedBars, bytes, trim1 } from '#/components/ui'

/**
 * Four tiles: what a first glance at a sandbox should tell you.
 *
 * Each pairs UTILISATION with the SATURATION signal for that resource -- the USE
 * method, narrowed to a machine that runs a coding agent. Utilisation on its own
 * is the less useful half, because the states that hurt here are not "busy":
 *
 *   - CPU     throttling. A quota'd container crawls at what looks like modest
 *             utilisation, and nothing else on the page explains why.
 *   - RAM     OOM kills. §2.4 makes that the blast-radius event -- it takes
 *             every session in the workspace, including a sibling's unattended
 *             run.
 *   - NET     errors and drops. Agents pull packages constantly, and this is
 *             the difference between "slow" and "broken".
 *
 * A string in place of the data is the control plane's own message; the
 * commonest is a workspace with no running task, which is a state and not a
 * failure.
 */
export function Resources({ data }: { data: ResourceUsage | string }) {
  if (typeof data === 'string') {
    return (
      <Panel>
        <p className="text-sm text-neutral-500">{data}</p>
      </Panel>
    )
  }

  const { cpu, memory, disk, network } = data
  const mb = (n: number, digits = 1) => bytes(n * 1024 * 1024, digits)

  // CPU usage in the process sense: processor time consumed per unit of
  // wall-clock time. Four CPU-seconds over ten seconds is 40%, and 100% means
  // one logical CPU fully occupied -- so this exceeds 100% on a multi-core task
  // and a build saturating four cores reads 400%.
  //
  // The BAR is the same quantity against the vCPU count, which is the ceiling
  // the number itself does not carry. Together they say "400%, and that is four
  // of ten".
  const cpuUsed = cpu.cores * 100
  const cpuOfCapacity = cpu.cpus > 0 ? (cpu.cores / cpu.cpus) * 100 : 0

  const memPct = memory.total_mb > 0 ? (memory.anon_mb / memory.total_mb) * 100 : 0
  const diskPct = disk.total_mb > 0 ? (disk.used_mb / disk.total_mb) * 100 : 0

  return (
    <Panel>
      <div className="grid grid-cols-1 gap-6 sm:grid-cols-2 lg:grid-cols-4">
        <Metric
          label="cpu"
          available={cpu.available}
          bar={cpuOfCapacity}
          value={trim1(cpuUsed)}
          alarm={
            <Alarm on={cpu.throttled_percent > 1}>
              throttled {cpu.throttled_percent.toFixed(0)}%
            </Alarm>
          }
          chart={cpu.history ? <Sparkline data={cpu.history} /> : null}
        />

        <Metric
          label="ram"
          available={memory.available}
          bar={memPct}
          value={trim1(memPct)}
          sub={
            <>
              {/* Anonymous memory, not memory.current: page cache is reclaimable
                  and counting it would overstate the pressure. */}
              {mb(memory.anon_mb)}, {mb(memory.total_mb)} total
            </>
          }
          alarm={
            <Alarm on={memory.oom_kills > 0}>
              {memory.oom_kills} OOM {memory.oom_kills === 1 ? 'kill' : 'kills'}
            </Alarm>
          }
          chart={
            memory.history ? (
              <StackedBars data={memory.history} total={memory.total_mb} />
            ) : null
          }
        />

        <Metric
          label="disk"
          available={disk.available}
          bar={diskPct}
          value={trim1(diskPct)}
          sub={
            <>
              {mb(disk.used_mb, 0)}, {mb(disk.total_mb, 0)} total
            </>
          }
        />

        <Metric
          label="net"
          available={network.available}
          rawValue={
            <span className="inline-flex gap-3">
              <span className="text-cyan-400">↓ {bytes(network.rx_bytes_per_sec)}/s</span>
              <span className="text-fuchsia-400">↑ {bytes(network.tx_bytes_per_sec)}/s</span>
            </span>
          }
          sub={
            <>
              ↑ {bytes(network.tx_total)}, ↓ {bytes(network.rx_total)},{' '}
              {bytes(network.rx_total + network.tx_total)} total
            </>
          }
          alarm={
            <Alarm on={network.errors + network.drops > 0}>
              {network.errors + network.drops} err/drop
            </Alarm>
          }
          chart={
            <DualLine a={network.rx_history ?? []} b={network.tx_history ?? []} />
          }
        />
      </div>
    </Panel>
  )
}

/**
 * A saturation figure that only appears once it means something.
 *
 * "0 OOM kills" and "throttled 0%" are the normal case and would be three tiles
 * of noise on every page view; the whole value of these numbers is that they are
 * absent until they are not. Amber rather than red because none of them is an
 * outage on its own -- they are the explanation for one.
 */
function Alarm({ on, children }: { on: boolean; children: React.ReactNode }) {
  if (!on) return null
  return <span className="text-amber-400">{children}</span>
}

/**
 * `available: false` is rendered as "unknown", never as a zero. The local driver
 * on darwin has no cgroups, and a confident 0% there would be the console
 * stating something false rather than admitting it could not look.
 *
 * The bar sits beside the LABEL rather than the number, so the four tiles line
 * up as a row of gauges that can be read across without stopping at each figure.
 */
function Metric({
  label,
  available,
  value,
  rawValue,
  sub,
  alarm,
  bar,
  chart,
}: {
  label: string
  available: boolean
  /** The percentage, without its sign -- "used" is appended and styled down. */
  value?: string
  /** For a tile whose headline is not a percentage. */
  rawValue?: React.ReactNode
  sub?: React.ReactNode
  alarm?: React.ReactNode
  bar?: number
  chart?: React.ReactNode
}) {
  return (
    <div className="space-y-1.5">
      <div className="flex items-center gap-2">
        <span className="text-xs tracking-wide text-neutral-500 uppercase">{label}</span>
        {bar === undefined ? null : (
          <span className="w-16 shrink-0">
            <Bar percent={bar} />
          </span>
        )}
      </div>

      {available ? (
        <>
          <div className="font-mono text-lg text-neutral-100">
            {rawValue ?? (
              <>
                {value}%<span className="ml-1 text-xs font-light text-neutral-500">used</span>
              </>
            )}
          </div>
          {sub ? <div className="font-mono text-xs text-neutral-500">{sub}</div> : null}
          {alarm ? <div className="font-mono text-xs">{alarm}</div> : null}
          {chart}
        </>
      ) : (
        <div className="font-mono text-lg text-neutral-600">unknown</div>
      )}
    </div>
  )
}
