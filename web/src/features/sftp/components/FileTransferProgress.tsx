import { useTranslation } from 'react-i18next'
import { formatFileSize } from '../../../lib/utils'
import type { ActiveFileTransfer } from '../types'

export function FileTransferProgress({transfer,onCancel}:{transfer:ActiveFileTransfer;onCancel:()=>void}){
	const {t}=useTranslation()
	const percent=transfer.total>0?Math.min(100,Math.round(transfer.loaded/transfer.total*100)):0
	return <div className={`file-transfer-progress ${transfer.total>0?'':'indeterminate'}`} role="progressbar" aria-valuemin={0} aria-valuemax={transfer.total||undefined} aria-valuenow={transfer.total>0?transfer.loaded:undefined}>
		<div><span title={transfer.name}>{transfer.index&&transfer.count?`${transfer.index}/${transfer.count} · `:''}{transfer.name}</span><b>{formatFileSize(transfer.loaded)}{transfer.total>0?` / ${formatFileSize(transfer.total)}`:''}</b><button type="button" onClick={onCancel}>{t('common.cancel')}</button></div>
		<i><em style={transfer.total>0?{width:`${percent}%`}:undefined}/></i>
	</div>
}
