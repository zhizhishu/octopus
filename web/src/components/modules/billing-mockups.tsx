'use client';

import { useState, type ReactNode } from 'react';
import { useTranslations } from 'next-intl';
import { Wallet as WalletIcon, ArrowUpRight, ShieldCheck } from 'lucide-react';
import { useAuthStore } from '@/api/endpoints/user';
import { PageWrapper } from '@/components/common/PageWrapper';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Badge } from '@/components/ui/badge';
import { Table, TableHeader, TableBody, TableRow, TableHead, TableCell } from '@/components/ui/table';
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription } from '@/components/ui/dialog';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { useSettingStore, type Locale } from '@/stores/setting';

const panel = 'rounded-3xl border border-border bg-card p-6';
const selectStyle = 'h-9 rounded-xl border border-input bg-background px-3 text-sm';
const people = ['alice', 'bob', 'carol'];

function Shell({ title, children }: { title: string; children: ReactNode }) {
    const t = useTranslations('billingMock');
    const { locale, setLocale } = useSettingStore();
    return <div className="h-full overflow-y-auto pb-16"><PageWrapper className="space-y-5"><div className="flex items-start justify-between"><div><h1 className="text-2xl font-semibold tracking-tight">{title}</h1><p className="mt-1 text-sm text-muted-foreground">{t('subtitle')}</p></div><div className="flex items-center gap-3"><Badge variant="outline">{t('demo')}</Badge><Select value={locale} onValueChange={v => setLocale(v as Locale)}><SelectTrigger aria-label="Language" className="w-32 rounded-xl"><SelectValue /></SelectTrigger><SelectContent><SelectItem value="zh_hans">简体中文</SelectItem><SelectItem value="zh_hant">繁體中文</SelectItem><SelectItem value="en">English</SelectItem></SelectContent></Select></div></div>{children}</PageWrapper></div>;
}

function Picker({ value, onChange, values, label }: { value: string; onChange: (v: string) => void; values: string[][]; label: string }) {
    return <select aria-label={label} className={selectStyle} value={value} onChange={e => onChange(e.target.value)}>{values.map(([v, text]) => <option key={v} value={v}>{text}</option>)}</select>;
}


const ledger = [
    ['2026-06-14 14:32:08', 'alice', 'charge', '-0.0240', '128.4600', 'log_demo_0824', 'claude-sonnet-4.6'],
    ['2026-06-14 14:28:42', 'bob', 'topup', '+50.0000', '74.6000', 'ord_demo_1003', 'Stripe'],
    ['2026-06-14 13:15:20', 'carol', 'redeem', '+20.0000', '32.8000', 'rdm_demo_0042', 'redeemNote'],
    ['2026-06-14 12:06:11', 'alice', 'refund', '+0.1800', '128.4840', 'log_demo_0812', 'refundNote'],
    ['2026-06-14 10:40:03', 'bob', 'adjust', '+5.0000', '24.6000', 'adj_demo_0018', 'adjustNote'],
    ['2026-06-14 09:21:56', 'alice', 'topup', '+100.0000', '128.3040', 'ord_demo_1001', 'epay'],
];

export function Wallet() {
    const t = useTranslations('billingMock');
    const user = useAuthStore(s => s.user);
    const admin = user?.role === 'admin';
    const [tab, setTab] = useState('ledger');
    const [person, setPerson] = useState('all');
    const [kind, setKind] = useState('all');
    const [open, setOpen] = useState(false);
    const [message, setMessage] = useState('');
    const [code, setCode] = useState('');
    const rows = ledger.filter(r => (admin ? person === 'all' || r[1] === person : r[1] === user?.username) && (kind === 'all' || r[2] === kind));
    return <Shell title={t('wallet')}>
        {admin ? <div className="flex items-center justify-between"><div className="flex gap-1 rounded-xl border p-1">{['ledger', 'orders', 'reconciliation'].map(x => <Button key={x} variant={tab === x ? 'secondary' : 'ghost'} onClick={() => setTab(x)}>{t(x)}</Button>)}</div><Button onClick={() => setOpen(true)}><ArrowUpRight className="size-4" />{t('adjustBalance')}</Button></div> : <><div className="grid gap-4 md:grid-cols-2"><section className={panel}><WalletIcon className="mb-3 size-5 text-muted-foreground" /><p className="text-sm text-muted-foreground">{t('balance')}</p><p className="my-3 text-4xl font-semibold">$128.46 <span className="text-sm font-normal text-muted-foreground">USD</span></p><p className="text-xs text-muted-foreground">alice · {t('balanceNote')}</p></section><section className={panel}><div className="flex justify-between"><h2 className="font-medium">{t('monthly')}</h2><Badge variant="secondary">{t('active')}</Badge></div><p className="mt-5 text-2xl font-semibold">Pro · $10.00 / {t('day')}</p><p className="mt-3 text-sm text-muted-foreground">{t('monthlyNote')}</p><div className="mt-4 h-2 rounded-full bg-muted"><div className="h-2 w-1/3 rounded-full bg-primary" /></div></section></div><section className={panel}><h2 className="mb-4 font-medium">{t('addFunds')}</h2><div className="flex gap-3"><Input className="max-w-sm" value={code} onChange={e => setCode(e.target.value)} placeholder={t('redeemPlaceholder')} aria-label={t('redeem')} /><Button onClick={() => setMessage(t(code.trim() ? 'invalidCode' : 'required'))}>{t('redeem')}</Button><span title={t('phaseTwo')} tabIndex={0}><Button disabled variant="outline">{t('onlineTopup')}</Button></span></div>{message && <p role="status" className="mt-3 text-sm text-muted-foreground">{message}</p>}</section></>}
        {tab === 'ledger' && <section className="overflow-hidden rounded-3xl border bg-card"><div className="flex flex-wrap items-center gap-3 border-b p-5"><h2 className="mr-auto font-medium">{t(admin ? 'ledger' : 'ownLedger')}</h2>{admin && <Picker label={t('user')} value={person} onChange={setPerson} values={[[ 'all', t('allUsers')], ...people.map(x => [x, x])]} />}<Picker label={t('type')} value={kind} onChange={setKind} values={[[ 'all', t('allTypes')], ...['topup', 'charge', 'refund', 'adjust', 'redeem'].map(x => [x, t(x)])]} /><Input className="w-40" type="date" aria-label={t('from')} defaultValue="2026-06-01" /><span className="text-muted-foreground">—</span><Input className="w-40" type="date" aria-label={t('to')} defaultValue="2026-06-30" /></div><Table><TableHeader><TableRow>{['time', ...(admin ? ['user'] : []), 'type', 'amount', 'balanceAfter', 'reference', 'note'].map(x => <TableHead key={x}>{t(x)}</TableHead>)}</TableRow></TableHeader><TableBody>{rows.map(r => <TableRow key={r[5]}><TableCell className="text-muted-foreground">{r[0]}</TableCell>{admin && <TableCell className="font-medium">{r[1]}</TableCell>}<TableCell><Badge variant="secondary">{t(r[2])}</Badge></TableCell><TableCell className={r[3].startsWith('+') ? 'font-mono text-emerald-600' : 'font-mono'}>{r[3]}</TableCell><TableCell className="font-mono">${r[4]}</TableCell><TableCell className="font-mono text-xs text-muted-foreground">{r[5]}</TableCell><TableCell>{['redeemNote', 'refundNote', 'adjustNote', 'epay'].includes(r[6]) ? t(r[6]) : r[6]}</TableCell></TableRow>)}</TableBody></Table><div className="border-t px-5 py-4 text-xs text-muted-foreground">{t('recordCount', { count: rows.length })} · USD</div></section>}
        {admin && tab === 'orders' && <section className="overflow-hidden rounded-3xl border bg-card"><div className="border-b p-5 font-medium">{t('orders')} <span className="ml-2 text-sm font-normal text-muted-foreground">{t('ordersNote')}</span></div><Table><TableHeader><TableRow>{['orderId', 'user', 'amount', 'channel', 'status', 'time'].map(x => <TableHead key={x}>{t(x)}</TableHead>)}</TableRow></TableHeader><TableBody>{['paid', 'pending', 'failed', 'refunded'].map((s, i) => <TableRow key={s}><TableCell className="font-mono text-xs">ord_demo_{1003 - i}</TableCell><TableCell>{people[i % 3]}</TableCell><TableCell className="font-medium">${[50, 100, 20, 30][i]}.00</TableCell><TableCell>{['Stripe', t('epay'), t('redeem'), 'Stripe'][i]}</TableCell><TableCell><Badge variant={s === 'paid' ? 'default' : 'secondary'}>{t(s)}</Badge></TableCell><TableCell className="text-muted-foreground">2026-06-{14 - i} 14:28:42</TableCell></TableRow>)}</TableBody></Table></section>}
        {admin && tab === 'reconciliation' && <section className={panel}><div className="flex items-center justify-between"><div><h2 className="font-medium">{t('reconciliation')}</h2><p className="mt-1 text-sm text-muted-foreground">{t('reconcileNote')}</p></div><Input type="month" aria-label={t('month')} defaultValue="2026-06" className="w-44" /></div><div className="my-8 grid grid-cols-3 divide-x">{[['ledgerCharges', '$86.4000'], ['relayCosts', '$86.4000'], ['difference', '$0.0000']].map(([k, v]) => <div key={k} className="px-6"><p className="text-sm text-muted-foreground">{t(k)}</p><p className="mt-3 text-3xl font-semibold tabular-nums">{v}</p></div>)}</div><div className="flex items-center gap-3 rounded-2xl bg-emerald-500/10 p-5 text-emerald-700"><ShieldCheck className="size-6" /><div><p className="font-medium">{t('matched')}</p><p className="mt-1 text-sm">{t('matchedNote')}</p></div><Badge variant="outline" className="ml-auto">USD</Badge></div></section>}
        <p className="text-xs text-muted-foreground">{t('mockNote')}</p>
        <Dialog open={open} onOpenChange={setOpen}><DialogContent className="rounded-3xl"><DialogHeader><DialogTitle>{t('adjustBalance')}</DialogTitle><DialogDescription>{t('adjustDescription')}</DialogDescription></DialogHeader><form className="space-y-5" onSubmit={e => { e.preventDefault(); setOpen(false); setMessage(t('previewSaved')); }}><label className="block space-y-2 text-sm"><span>{t('user')}</span><select className={`${selectStyle} w-full`}>{people.map(p => <option key={p}>{p}</option>)}</select></label><label className="block space-y-2 text-sm"><span>{t('amount')} (USD) *</span><Input required type="number" step="0.0001" placeholder="+10.00" /></label><label className="block space-y-2 text-sm"><span>{t('reason')} *</span><Input required placeholder={t('reasonPlaceholder')} /></label><p className="rounded-xl bg-muted p-3 text-xs text-muted-foreground">{t('adjustHint')}</p><div className="flex justify-end gap-2"><Button type="button" variant="outline" onClick={() => setOpen(false)}>{t('cancel')}</Button><Button type="submit">{t('confirm')}</Button></div></form></DialogContent></Dialog>
    </Shell>;
}
