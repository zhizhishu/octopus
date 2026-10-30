import { useRef, useState } from 'react';
import { useTranslations } from 'next-intl';
import { ChevronDown, FileSpreadsheet, HelpCircle, Loader2, Upload } from 'lucide-react';
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
    const t = useTranslations('channel.importCSV');
    const importCSV = useImportChannelCSV();
    const [file, setFile] = useState<File | null>(null);
    const [replaceKey, setReplaceKey] = useState(false);
    const [result, setResult] = useState<ChannelCSVImportResult | null>(null);
    const [showHelp, setShowHelp] = useState(false);

    const handleImport = async () => {
        if (!file) {
            toast.warning(t('needFile'));
            return;
        }
        try {
            const data = await importCSV.mutateAsync({ file, replaceKey });
            setResult(data);
            toast.success(t('done', { created: data.created, updated: data.updated, failed: data.failed }));
            if (inputRef.current) inputRef.current.value = '';
            setFile(null);
        } catch (error) {
            toast.error(error instanceof Error ? error.message : t('failed'));
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
                    {/* Title + help toggle */}
                    <div className="flex flex-wrap items-center justify-between gap-2">
                        <h3 className="text-sm font-semibold text-card-foreground">{t('title')}</h3>
                        <button
                            type="button"
                            onClick={() => setShowHelp(!showHelp)}
                            className="inline-flex items-center gap-1 rounded-lg px-2 py-1 text-xs text-muted-foreground hover:text-foreground hover:bg-muted/50 transition-colors"
                        >
                            <HelpCircle className="size-3" />
                            <span>{t('help')}</span>
                            <ChevronDown className={cn('size-3 transition-transform duration-200', showHelp && 'rotate-180')} />
                        </button>
                    </div>

                    {/* Collapsible help section */}
                    {showHelp && (
                        <div className="mt-2 rounded-xl border border-border/60 bg-muted/20 p-3 space-y-2">
                            <p className="text-xs leading-relaxed text-muted-foreground">
                                {t('formatHint')}
                            </p>
                            <div className="flex justify-end">
                                <Button
                                    type="button"
                                    variant="outline"
                                    size="sm"
                                    onClick={() => navigator.clipboard?.writeText(SAMPLE_CSV).then(() => toast.success(t('sampleCopied')))}
                                >
                                    {t('copySample')}
                                </Button>
                            </div>
                            <p className="text-xs leading-relaxed text-muted-foreground">
                                {t('afterImportHint')}
                            </p>
                        </div>
                    )}

                    {/* Always-visible operations */}
                    <div className="mt-3 grid gap-3 md:grid-cols-[minmax(0,1fr)_auto]">
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
                            {importCSV.isPending ? t('importing') : t('start')}
                        </Button>
                    </div>

                    <label className="mt-3 flex items-center gap-2 text-xs text-muted-foreground">
                        <Switch checked={replaceKey} onCheckedChange={setReplaceKey} />
                        {t('replaceKey')}
                    </label>

                    {result && (
                        <div className="mt-4 rounded-xl border bg-background/60 p-3 text-xs">
                            <div className="grid grid-cols-2 gap-2 sm:grid-cols-5">
                                {[
                                    [t('total'), result.total],
                                    [t('created'), result.created],
                                    [t('updated'), result.updated],
                                    [t('skipped'), result.skipped],
                                    [t('failedCount'), result.failed],
                                ].map(([label, value]) => (
                                    <div key={label} className={cn('rounded-lg border px-3 py-2', label === t('failedCount') && result.failed > 0 ? 'border-destructive/30 text-destructive' : 'border-border')}>
                                        <div className="text-muted-foreground">{label}</div>
                                        <div className="mt-1 text-base font-semibold">{value}</div>
                                    </div>
                                ))}
                            </div>
                            {failedRows.length > 0 && (
                                <div className="mt-3 space-y-1 text-destructive">
                                    {failedRows.slice(0, 5).map((row) => (
                                        <div key={`${row.row}-${row.name}`}>{t('rowError', { row: row.row, name: row.name || t('unnamed'), error: row.error ?? '' })}</div>
                                    ))}
                                    {failedRows.length > 5 && <div>{t('moreFailed', { count: failedRows.length - 5 })}</div>}
                                </div>
                            )}
                        </div>
                    )}
                </div>
            </div>
        </section>
    );
}
