import Markdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { useTranslation } from 'react-i18next'

import { CopyablePre } from './CopyButton'

export function MarkdownMessage({content,scope='chat'}:{content:string;scope?:'chat'|'skills'}){
	const {t}=useTranslation()
	return <Markdown skipHtml remarkPlugins={[remarkGfm]} components={{
		a:({href,children})=><a href={href} target="_blank" rel="noopener noreferrer">{children}</a>,
		img:({alt})=><span className="markdown-image-blocked">{t(`${scope}.blockedImage`,{alt:alt||t('common.image')})}</span>,
		pre:({children})=><CopyablePre>{children}</CopyablePre>,
	}}>{content}</Markdown>
}
