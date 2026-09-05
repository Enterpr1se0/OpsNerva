import i18n from '../../../lib/i18n'
import { displayValue, toolCollectionPreviewItems } from '../payload'

export function CompactTable({title,columns,rows}:{title:string;columns:string[];rows:Array<Array<unknown>>}){
  const visibleRows=rows.slice(0,toolCollectionPreviewItems)
  if(rows.length>visibleRows.length)visibleRows.push([i18n.t('tool.previewItemsOmitted',{count:rows.length-visibleRows.length})])
  return <div className="tool-compact-table"><span>{title}</span><div className="tool-table-scroll"><table><thead><tr>{columns.map(column=><th key={column}>{column}</th>)}</tr></thead><tbody>{visibleRows.map((row,index)=><tr key={index}>{row.map((value,column)=><td key={column}>{displayValue(value)}</td>)}</tr>)}</tbody></table></div></div>
}
