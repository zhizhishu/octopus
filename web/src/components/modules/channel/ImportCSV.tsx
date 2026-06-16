import { useRef, useState } from 'react';
import { FileSpreadsheet, Loader2, Upload } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Switch } from '@/components/ui/switch';
import { toast } from '@/components/common/Toast';
import { cn } from '@/lib/utils';
import { useImportChannelCSV, type ChannelCSVImportResult } from '@/api/endpoints/channel';

const SAMPLE_CSV = `type,name,baseURL,apiKey,supportedModels,defaultTestModel
openai,OpenAI GPT,https://api.openai.com/v1,sk-xxx,gpt-4|gpt-3.5-turbo,gpt-4
anthropic,Anthropic Claude,https://api.anthropic.com,claude-xxx,claude-3-opus|claude-3-sonnet,claude-3-opus
deepseek,DeepSeek AI,https://api.deepseek.com,sk-xxx,deepseek-chat|deepseek-coder,deepseek-chat
deepseek_anthropic,DeepSeek Anthropic,https://api.deepseek.com/anthropic,sk-xxx,deepseek-chat|deepseek-coder,deepseek-chat`;

export function ChannelImportCSV() {
    const inputRef = useRef<HTMLInputElement | null>(null);
    const importCSV = useImportChannelCSV();
    const [file, setFile] = useState<File | null>(null);
    const [replaceKey, setReplaceKey] = useState(false);
    const [result, setResult] = useState<ChannelCSVImportResult | null>(null);

    const handleImport = async () => {
        if (!file) {
            toast.warning('先选 CSV 文件，别空手套渠道啦');
            return;
        }
        try {
            const data = await importCSV.mutateAsync({ file, replaceKey });
            setResult(data);
            toast.success(`CSV 导入完成：新增 ${data.created}，更新 ${data.updated}，失败 ${data.failed}`);
            if (inputRef.current) inputRef.current.value = '';
            setFile(null);
        } catch (error) {
            toast.error(error instanceof Error ? error.message : 'CSV 导入失败');
        }
    };

    const failedRows = result?.rows.filter((row) => row.action === 'failed') ?? [];

    return (
        <section className="mb-5 overflow-hidden rounded-2xl border border-dashed border-primary/30 bg-gradient-to-br from-primary/8 via-card to-accent/8 p-4">
            <div className="flex items-start gap-3">
                <div className="rounded-2xl bg-primary/10 p-2 text-primary">
                    <FileSpreadsheet className="size-5" />
                </div>
                <div className="min-w-0 flex-1">
                    <div className="flex flex-wrap items-center justify-between gap-2">
                        <div>
                            <h3 className="text-sm font-semibold text-card-foreground">CSV 批量导入渠道</h3>
                            <p className="mt-1 text-xs text-muted-foreground">
                                每行一个渠道：type,name,baseURL,apiKey,supportedModels,defaultTestModel；多个模型用 | 分隔。CSV 里的 supportedModels 会作为已启用模型写入；后续同步发现只进候选，不会自动扩权。
                            </p>
                        </div>
                        <Button
                            type="button"
                            variant="outline"
                            size="sm"
                            onClick={() => navigator.clipboard?.writeText(SAMPLE_CSV).then(() => toast.success('示例 CSV 已复制'))}
                        >
                            复制示例
                        </Button>
                    </div>

                    <div className="mt-4 grid gap-3 md:grid-cols-[minmax(0,1fr)_auto]">
                        <Input
                            ref={inputRef}
                            type="file"
                            accept=".csv,text/csv"
                            onChange={(event) => {
                                setFile(event.target.files?.[0] ?? null);
                                setResult(null);
                            }}
                        />
                        <Button type="button" onClick={handleImport} disabled={importCSV.isPending}>
                            {importCSV.isPending ? <Loader2 className="size-4 animate-spin" /> : <Upload className="size-4" />}
                            {importCSV.isPending ? '导入中' : '开始导入'}
                        </Button>
                    </div>

                    <label className="mt-3 flex items-center gap-2 text-xs text-muted-foreground">
                        <Switch checked={replaceKey} onCheckedChange={setReplaceKey} />
                        替换同名渠道旧 key（默认关闭，避免把线上 key 一键扫没）
                    </label>
                    <p className="mt-2 text-xs text-muted-foreground">
                        导入完成后只基于已启用模型刷新价格占位与模型池；自动同步拿到的新模型会留在候选区，管理员手动选择后才可进入方案和路由。
                    </p>

                    {result && (
                        <div className="mt-4 rounded-xl border bg-background/60 p-3 text-xs">
                            <div className="grid grid-cols-2 gap-2 sm:grid-cols-5">
                                {[
                                    ['总行数', result.total],
                                    ['新增', result.created],
                                    ['更新', result.updated],
                                    ['跳过', result.skipped],
                                    ['失败', result.failed],
                                ].map(([label, value]) => (
                                    <div key={label} className={cn('rounded-lg border px-3 py-2', label === '失败' && result.failed > 0 ? 'border-destructive/30 text-destructive' : 'border-border')}>
                                        <div className="text-muted-foreground">{label}</div>
                                        <div className="mt-1 text-base font-semibold">{value}</div>
                                    </div>
                                ))}
                            </div>
                            {failedRows.length > 0 && (
                                <div className="mt-3 space-y-1 text-destructive">
                                    {failedRows.slice(0, 5).map((row) => (
                                        <div key={`${row.row}-${row.name}`}>第 {row.row} 行 {row.name || '(未命名)'}：{row.error}</div>
                                    ))}
                                    {failedRows.length > 5 && <div>还有 {failedRows.length - 5} 行失败，先别装看不见。</div>}
                                </div>
                            )}
                        </div>
                    )}
                </div>
            </div>
        </section>
    );
}
